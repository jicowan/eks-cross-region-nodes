# Gap Analysis: Implemented Code vs. PRDs

**Date:** 2026-06-05
**Scope:** Reconciles [PRD-cross-region-nodes.md](./PRD-cross-region-nodes.md) (same-account) and [PRD-cross-account-nodes.md](./PRD-cross-account-nodes.md) (cross-account) against the code in this repo.

**Guiding constraint (from product owner):** there is **one `xrn-install` binary** and **one `xrnctl` binary** serving both same-account/cross-region and cross-account topologies. Cross-account is a set of *additive flags/behaviors* on the same commands, not a fork. This doc therefore tags each gap as **shared** (helps both scenarios), **same-account** (cross-region PRD only), or **cross-account** (cross-account PRD only) so we can sequence work without painting ourselves into a fork.

---

## 1. What exists today (Jun 3 commit, builds clean, all tests pass)

### `xrn-install` (`cmd/xrn-install`, `pkg/{discovery,preflight,bootstrap,patch}`)

| Capability | State |
|---|---|
| Subcommands `init`, `preflight`, `discover`, `version` | ✅ implemented |
| Cluster discovery (`eks:DescribeCluster`) | ✅ `pkg/discovery` |
| Node metadata from IMDS (instance-id, region, AZ, VPC, subnet, IP, type, account) | ✅ `pkg/discovery` |
| 9 preflight checks (IMDS, cross-region, DNS, endpoint reachable, hop-limit, access-entry [warn], CIDR overlap, RemoteNetworkConfig [warn], CSR-approver [warn]) | ✅ `pkg/preflight` |
| Patch: cloud-provider→empty, hostname-override→instance-id, topology labels, providerID→eks-hybrid, kubeconfig region | ✅ `pkg/patch` |
| Bootstrap: `nodeadm init` (AL2023) with skip-if-already-bootstrapped | ✅ `pkg/bootstrap` |
| Kubelet restart | ✅ `pkg/bootstrap` |
| Structured exit codes (10–19) | ✅ partial (see gaps) |

### `xrnctl` (`cmd/xrnctl`, `pkg/registry`)

| Capability | State |
|---|---|
| Subcommands `add-region`, `remove-region`, `list-regions`, `verify`, `version` | ✅ implemented |
| ConfigMap (`aws-node-vpc-cidrs`) read/write with `registry.json` + `exclude-snat-cidrs` | ✅ `pkg/registry` |
| CIDR overlap rejection on add | ✅ |
| ENIConfig CRUD (opt-in via `--with-eniconfigs`) | ✅ |
| RemoteNetworkConfig update on add (best-effort, warns about node reap) | ✅ |
| `remove-region` refuses if nodes still present | ✅ |
| `verify` drift detection (ConfigMap vs ENIConfig vs nodes) | ✅ |
| Cluster-account VPC CIDRs auto-included in exclusion list | ✅ |

### Phase 2 (ConfigMap SNAT watcher)

Lives in **amazon-vpc-cni-k8s**, not this repo. Implemented and submitted as a PR (commit `8ed3dc57` on branch `feature/configmap-snat-watcher`), **not yet merged**. Gated behind `AWS_VPC_K8S_CNI_ENABLE_DYNAMIC_SNAT_CFG=true`.

### Test coverage

| Package | Tests |
|---|---|
| `pkg/patch` | ✅ good (env, providerID, kubeconfig, idempotency, ApplyAll) |
| `pkg/preflight` | ✅ good (overlap, IMDS, cross-region, Results logic, DNS) |
| `pkg/registry` | ✅ good (overlap, dedupe, serialization, merge, mapKeys) |
| `pkg/discovery` | ❌ none |
| `pkg/bootstrap` | ⚠️ exists but AL2023-only |
| `cmd/*` (flag parsing) | ❌ none |

---

## 2. Gaps vs. the same-account PRD (cross-region)

| # | Gap | PRD ref | Tag | Severity |
|---|---|---|---|---|
| SA-1 | ~~AMI-agnostic bootstrap~~ **CLOSED / out of scope (2026-06-05).** AL2 is explicitly out of scope (product owner). Bottlerocket's bootstrap is settings/TOML-based — structurally different from `nodeadm` and from the file-patching model `xrn-install` uses; deferred to a future workstream if ever needed. **AL2023/`nodeadm` is the only supported AMI.** | §4 Phase 3, §12 | — | n/a |
| SA-2 | **`--mtu` flag.** Not implemented. Note: MTU is an aws-node *DaemonSet* env var, so the installer can't set it per-node; this really belongs to the split-DaemonSet pattern (PRD §11, deferred to "Phase 5"). | §9 | same-account | Low — arguably mis-specified; revisit |
| SA-3 | **`discover` has no tests; `cmd` flag parsing has no tests.** | §4 Phase 3 test plan | shared | Medium |
| SA-4 | **Idempotency not fully verified.** `patch` is idempotent (tested). `RunNodeadmIfNeeded` skips on existing kubeconfig. But `init` end-to-end idempotency (re-run on healthy node) isn't covered by a test. | §4 Phase 3 test plan | shared | Medium |
| SA-5 | **CI matrix.** `.github/workflows/release.yml` exists but no AL2/AL2023/Bottlerocket bootstrap test matrix. | §4 Phase 3 test plan | shared | Low |
| SA-6 | **`preflight` check 7 (ENIConfig exists for AZ) and check 8 (cluster VPC TCP probe)** from PRD §6 table are not implemented. Current preflight has 9 checks but not these two exactly. ENIConfig check is moot when ENIConfigs are opt-in (the default skips them). | §6 | same-account | Low |

## 3. Gaps vs. the cross-account PRD

| # | Gap | PRD ref | Tag | Severity |
|---|---|---|---|---|
| XA-1 | **Bootstrap timing (cloud-boothook).** Cross-account requires patches applied *before* kubelet's first start. Current `xrn-install` runs post-boot (after nodeadm starts kubelet). Cross-account needs the cloud-boothook + systemd-drop-in pattern, OR `xrn-install` gains a "pre-kubelet" mode. | XA §5.1 | cross-account | High (blocker for XA) |
| XA-2 | **AssumeRole credential helper for kubelet.** Cross-account kubelet auth chains instance-role → cluster-account `XrnSatelliteNodeRole`. Current `patch.patchKubeconfig` only rewrites the region; it doesn't install an AssumeRole credential-process script. | XA §5.2 | cross-account | High (blocker for XA) |
| XA-3 | **`compute-type=hybrid` label** instead of `cross-region`. Hardcoded as `cross-region` in `pkg/bootstrap` NodeConfig template, `pkg/registry` node-selector queries, deploy templates. | XA §5.6 | shared (unification) | High |
| XA-4 | **`xrn.amazonaws.com/satellite-account` + region labels.** Not set by installer. Needed for per-(account,region) DS partitioning. | XA §5.4 | shared | Medium |
| XA-5 | **`aws-node-satellite` DaemonSet generation.** No template/render in this repo. Created by hand during testing. | XA §5.4 | cross-account | High |
| XA-6 | **`xrnctl add-satellite` / `remove-satellite` / `list-satellites`.** Cross-account uses these names; same-account uses `add-region` etc. Need to reconcile to one verb set. | XA §5.4.1 | shared | Medium |
| XA-7 | **`--cluster-account` / AssumeRole flags on installer.** Auto-detect account mismatch, error if flag absent. | XA §5.1 | cross-account | Medium |
| XA-8 | **Cross-VPC SG rule documentation/automation** (DNS, pod-port, kubelet). Validated manually today. | XA §5.5.5 | shared | Low (likely doc-only) |

---

## 4. The unification decisions (RESOLVED by product owner 2026-06-05)

### Q1: Label — `cross-region` for same-account, `hybrid` for cross-account (do NOT unify)

**Decision: the two labels are load-bearing for DaemonSet routing — keep both.** My earlier "unify to `hybrid`" recommendation was wrong given the DS model (Q3).

Rationale: EKS's stock `aws-node` (vpc-cni addon) nodeAffinity **already excludes `eks.amazonaws.com/compute-type=hybrid`** (hybrid nodes are meant to run Cilium, not VPC CNI) and does **not** exclude `cross-region` (a non-standard value). This gives free routing:

| Node | compute-type label | Stock `aws-node` runs? | CNI source |
|---|---|---|---|
| Cluster-VPC | (EKS default / unset) | yes | stock aws-node + Pod Identity |
| Same-account satellite (diff region) | `cross-region` | **yes** (not excluded) | stock aws-node + Pod Identity (works, same account) |
| Cross-account satellite | `hybrid` | **no** (excluded by stock default) | dedicated `aws-node-satellite-<acct>-<region>` DS, IMDS creds |

No edits to the EKS-managed `aws-node` DS are required in either case. The lab's earlier patch of `aws-node` to exclude `cross-region` was a cross-account-test artifact and is NOT part of the same-account design.

**Open empirical risk (resolve in live test):** this session we observed same-account `cross-region` nodes being reaped by CCM — but plausibly because we had broken their CNI (the aws-node exclusion patch → no CNI → NotReady → reaped). With an *unbroken* stock aws-node, a same-account `cross-region` satellite should stay Ready and survive (consistent with historical behavior the product owner reports). The live test confirms: leave stock aws-node alone, join a same-account `cross-region` node, verify it gets CNI, stays Ready, and is not reaped.

### Q2: `xrnctl` verbs — `add-satellite` (drop `add-region`)

**Decision: rename to `add-satellite`/`remove-satellite`/`list-satellites`. No backward-compat alias** (product owner: not needed). Add an `--account-id` flag (defaults to the cluster account when omitted ⇒ same-account/cross-region; differs ⇒ cross-account). The underlying `registry.Manager` already does the work; this is a CLI-surface rename + flag.

### Q3: DS model — by account (RESOLVED)

**Decision (product owner): instances in a DIFFERENT account from the cluster need their own `aws-node` DaemonSet; instances in the SAME account do NOT.**

- Same-account satellite → rides stock `aws-node` (Q1 routing makes this automatic via the `cross-region` label).
- Cross-account satellite → dedicated `aws-node-satellite-<acct>-<region>` DS (XA-5).

`xrnctl add-satellite`:
- same-account (`--account-id` == cluster account or omitted): update ConfigMap CIDRs + RemoteNetworkConfig + optional ENIConfigs. **No DS work.**
- cross-account (`--account-id` != cluster account): all of the above **plus** render+apply the dedicated DS, and (cross-account only) patch the stock `aws-node` NotIn to also exclude that satellite-account label so it doesn't double-schedule. (Detail for the cross-account phase.)

### Q4: Bootstrap timing — post-boot for same-account, pre-kubelet for cross-account

Same-account works with post-boot patch (`xrn-install init` after nodeadm starts kubelet — validated across many deployments). Cross-account needs pre-kubelet (cloud-boothook + sentinel — validated 2026-06-05). One binary supports both: keep post-boot `init` for same-account; cross-account phase adds a pre-kubelet entry point (e.g. `xrn-install patch` invoked from the boothook before the sentinel release). The patch logic is identical; only *when* it runs differs.

---

## 5. Recommended sequencing (single-binary safe)

Same-account/cross-region is the current focus. Cross-account follows after a testing pause.

**Same-account/cross-region (now):**
1. **Rename `xrnctl` verbs to `add-satellite`/`remove-satellite`/`list-satellites`** (Q2), add `--account-id` flag (unused logic-wise for same-account beyond defaulting). Drop `add-region`.
2. **Harden existing code** (SA-3, SA-4): discovery tests, cmd flag-parsing tests, end-to-end `init` idempotency test. No behavior change.
3. **Live-test same-account flow** end-to-end against the lab with stock `aws-node` left UNMODIFIED — confirm the `cross-region` node gets CNI, stays Ready, survives (resolves the Q1 empirical risk). This is the real proof the same-account design is correct.
4. **Pause for user testing.**

NOT doing in the same-account phase: label unification (Q1 — keep `cross-region`), DS template (cross-account only), boothook/AssumeRole (cross-account only).

**Cross-account (later):** XA-1/XA-2 (boothook + AssumeRole helper), XA-5 (DS template), XA-7 (installer `--cluster-account` + AssumeRole flags), and the cross-account branch of `add-satellite` (XA-6).

AMI-agnostic bootstrap (SA-1) is closed: AL2023/nodeadm only. AL2 out of scope; Bottlerocket deferred.

---

## 5b. Done in this pass (2026-06-05, branch `feature/same-account-hardening`)

- **Verb rename** `add-region`→`add-satellite`, `remove-region`→`remove-satellite`, `list-regions`→`list-satellites` (no backward-compat alias, per product owner). `--account-id` flag added; stored in `registry.json`; `list-satellites` shows an ACCOUNT column; `AddRegionResult.CrossAccount` reports same- vs cross-account.
- **`--profile` global flag** on all `xrnctl` subcommands → threads through `registry.NewManager` and every per-region AWS config load (`loadAWSConfig`). Lets one binary target different accounts.
- **`xrnctl setup-iam` subcommand** (`pkg/iamsetup`): creates the satellite node IAM role + instance profile (worker/CNI/ECR/SSM managed policies, EC2 trust) and/or the `HYBRID_LINUX` access entry. Idempotent (reuses existing role/profile/entry; verifies entry type). `--node-role-only` / `--access-entry-only` split the two halves for the cross-account two-account flow; `--node-role-arn` lets the access-entry half reference a role in another account.
- **Hardening:** `argValue` guards against trailing-flag panics; all parsers reject unknown flags; new tests for `cmd/xrn-install` and `cmd/xrnctl` flag parsing, `accountIDFromARN`, `account_id` omitempty, `splitComma`. `go vet` clean, cross-compiles linux/amd64+arm64.
- **Doc:** cross-account PRD §5.4.0 added — the three-authentications table clarifying that the access entry is per-node-role (both topologies) and only the dedicated DaemonSet is cross-account-specific.

Still TODO (this pass): live-test against the lab; `pkg/iamsetup` lacks unit tests (AWS-API-heavy; needs SDK seams or integration test).

## 5c. Live-test finding (2026-06-05): the reap discriminator is providerID timing, not the label

A clean same-account/cross-region node test (`i-0cc763678b88f930a`, eu-west-1, label `cross-region`, riding **stock aws-node**) settled the Q1 empirical question and corrected our model:

- The node gets the **same** `InvalidInstanceID.NotFound` from CCM's region-scoped EC2 lookups (the instance is in eu-west-1; CCM queries us-east-2). The tagging_controller spews errors for it just like a cross-account node.
- **It is NOT reaped.** `node_lifecycle_controller` logged 3 "skipping… within grace period" lines, then went **silent** — no "deleting node" at grace expiry.
- Its `providerID` is `eks-hybrid:///us-east-2/main/i-0cc763678b88f930a`.

**Conclusion:** what stops the lifecycle reap is the **`eks-hybrid:///` providerID being in place before the 2-minute grace expires** — not the `compute-type` label value, and not which DaemonSet runs. The earlier same-account reap (`i-0de3b89cd2c5099df` at 19:03) happened because that node's CNI was broken (we'd patched stock aws-node to exclude `cross-region`), so it never went Ready and the providerID patch/restart cycle didn't settle in time.

This means:
- **Same-account/cross-region** works on stock aws-node with the `cross-region` label, post-boot `xrn-install` — *provided the providerID patch lands within ~2 min* (it does, comfortably, when the CNI isn't broken). Validated.
- The `compute-type=hybrid` label's role in the cross-account path is about **DaemonSet routing** (keeping cross-account nodes off the Pod-Identity-bound stock aws-node), NOT about pacifying CCM. CCM is pacified by the providerID in both cases.
- The cross-account pre-kubelet boothook (XA-1) matters because cross-account has a *tighter* timing constraint and a credential problem, not because same-account's post-boot order is wrong.

## 5d. Cross-account phase built (2026-06-05, branch `feature/cross-account-nodes`)

Both tracks landed, back-to-back, all unit-tested. Not yet live-tested end-to-end.

**Track A — CNI:**
- **XA-5** `pkg/satellite`: `Render(Params)` produces SA + ClusterRoleBinding + DaemonSet (`aws-node-satellite-<acct>-<region>`) from the validated lab manifest. No Pod Identity SA; nodeAffinity scoped to compute-type=hybrid + satellite-account + region; AWS_REGION + SNAT CIDRs baked in; image registry/tags overridable. 93% coverage; output validated by `kubectl apply --dry-run=client` against the real API server.
- **XA-6** cross-account branch of `add-satellite`: when `--account-id != cluster account`, renders + applies the satellite DS via the dynamic client (create-or-update/idempotent); `--dry-run` prints instead. Same-account path unchanged. **Dropped the planned "patch stock aws-node NotIn" step** — stock aws-node already excludes compute-type=hybrid, so no mutation of the EKS-managed DS is needed.

**Track B — node bootstrap:**
- **XA-2** `pkg/patch`: `RenderCredentialHelper` (pure) + `installCredentialHelper` write the AssumeRole helper (session name = instance ID) and rewrite the kubeconfig exec to use it. `ApplyAll` gained a `*CrossAccount` param; same-account path (nil) is byte-for-byte unchanged.
- **XA-7** `cmd/xrn-install`: `--cluster-account-role-arn` + `--cluster-account-external-id` flags; `resolveCrossAccount` auto-detects account mismatch (instance acct vs cluster acct from ARN) and errors with a copy-pasteable hint if the flag is missing. `pkg/discovery` now exposes the cluster ARN + account.
- **XA-1** pre-kubelet ordering via **ExecStartPre-as-gate** (cleaner than the lab's separate-service-plus-sentinel): new `xrn-install patch` subcommand (discover + patch only, no nodeadm, no restart) is invoked from a kubelet `ExecStartPre` drop-in laid down by a cloud-boothook. kubelet's own `After=nodeadm-config` ordering is the gate. New `deploy/asg/userdata-cross-account.template.txt` + `README-cross-account.md`.

**Still owed:** end-to-end live test of a cross-account node through the full toolchain (setup-iam two-profile → add-satellite → ASG with the cross-account userdata). The `setup-iam --profile root` flow has not been run live yet.

## 6. Non-gaps (things the PRDs call for that ARE done)

- ConfigMap as canonical registry (not SSM) — done, matches PRD §5 decision.
- RemoteNetworkConfig auto-update with reap warning — done, matches PRD §13.1 + §14.
- CIDR overlap rejection — done both at install preflight and at `add-region`.
- ENIConfig opt-in (not default) — done, matches the "pods use node subnet by default" stance.
- Cluster-account VPC CIDRs auto-included in SNAT exclusion — done.

# arc-detective

arc-detective is a Kubernetes operator that automatically detects and diagnoses failures in [Actions Runner Controller (ARC)](https://github.com/actions/actions-runner-controller) self-hosted runner pods. When a runner pod crashes, gets OOMKilled, or gets stuck, arc-detective creates an Investigation custom resource containing a correlated timeline of events, collected logs, and a diagnosis with remediation advice.

## What it detects

arc-detective watches runner pods, EphemeralRunner CRs, and GitHub Actions job status to detect:

| Failure type | Trigger | Remediation hint |
|---|---|---|
| `pod-oomkilled` | Container OOMKilled or exit code 137 | Increase memory limits on the runner pod template |
| `pod-crashloop` | Container restart count >= 3 or CrashLoopBackOff | Check runner entrypoint script for post-job errors |
| `pod-stuck-terminating` | Pod stuck in Terminating phase | Force-delete the pod, check node autoscaler |
| `pod-init-timeout` | ImagePullBackOff or ErrImagePull during init | Pre-pull runner images or use a registry mirror |
| `runner-stuck-running` | EphemeralRunner still Running after job completed | Delete the stuck EphemeralRunner, check ARC controller |
| `runner-stuck-failed` | EphemeralRunner stuck in Failed | Delete the stuck EphemeralRunner (known ARC issue) |
| `job-stuck-queued` | GitHub job queued with no runner available | Check AutoScalingRunnerSet status and listener pod |

Each Investigation CR includes the pod's container statuses, phase transitions, Kubernetes events, collected logs, and optionally the GitHub workflow run and job details.

## Getting Started

### Prerequisites

- A Kubernetes cluster (v1.28+)
- [ARC v2](https://github.com/actions/actions-runner-controller) (gha-runner-scale-set) already installed and running
- A GitHub Personal Access Token with **Actions: Read** permission on the repositories you want to monitor
- kubectl configured to talk to your cluster
- Docker (for building the image)
- Go 1.24+ (for building from source)

### 1. Build and load the image

Build the controller image and push it to a registry your cluster can pull from:

```sh
make docker-build docker-push IMG=ghcr.io/yourorg/arc-detective:latest
```

Or if you're using a local Kind cluster for testing:

```sh
make docker-build IMG=arc-detective:latest
kind load docker-image arc-detective:latest --name your-cluster
```

### 2. Install CRDs and deploy

```sh
make install
make deploy IMG=ghcr.io/yourorg/arc-detective:latest
```

This creates the `arc-detective-system` namespace and deploys the controller.

### 3. Create a GitHub credentials secret

arc-detective needs a GitHub token to correlate runner failures with workflow runs. Create a secret in the `arc-detective-system` namespace:

```sh
kubectl create secret generic github-credentials \
  --namespace arc-detective-system \
  --from-literal=token=ghp_xxxxxxxxxxxxxxxxxxxx
```

The token needs **Actions: Read** permission on the repositories you want to monitor. A fine-grained PAT scoped to specific repositories is recommended.

### 4. Create a DetectiveConfig

Tell arc-detective which repositories to monitor:

```yaml
apiVersion: detective.arcdetective.io/v1alpha1
kind: DetectiveConfig
metadata:
  name: detective-config
  namespace: arc-detective-system
spec:
  repositories:
    - owner: myorg
      name: myrepo
    - owner: myorg
      name: another-repo
  githubAuth:
    type: pat
    secretName: github-credentials
  pollInterval: 30s
  logStorage:
    pvcName: ""
  retentionPeriod: 168h    # 7 days
```

```sh
kubectl apply -f detective-config.yaml
```

**`logStorage.pvcName`**: Set to a PersistentVolumeClaim name if you want collected pod logs to survive controller restarts. Leave empty (`""`) to use an emptyDir (logs are lost when the controller pod restarts).

**`pollInterval`**: How often to poll the GitHub Actions API for failed jobs. Default is 30s.

**`retentionPeriod`**: How long to keep Investigation CRs before they're cleaned up. Default is 7 days.

### 5. Verify it's running

```sh
kubectl get pods -n arc-detective-system
```

You should see the controller pod running. Check its logs:

```sh
kubectl logs -n arc-detective-system -l control-plane=controller-manager -f
```

### Viewing investigations

When arc-detective detects a failure, it creates an Investigation CR in the same namespace as the failed runner pod:

```sh
kubectl get investigations -A
```

```
NAMESPACE     NAME                                    PHASE      TRIGGER        FAILURE           AGE
arc-runners   pod-my-runner-abc123-runner-x7k9z       Complete   pod-oomkill    pod-oomkilled     5m
arc-runners   pod-my-runner-def456-runner-m2p4q       Complete   pod-deletion   pod-crashloop     12m
```

### Example investigation

Here is an actual Investigation CR from a runner pod that was OOMKilled:

```yaml
apiVersion: detective.arcdetective.io/v1alpha1
kind: Investigation
metadata:
  name: pod-my-runner-abc123-runner-x7k9z
  namespace: arc-runners
spec:
  trigger:
    type: pod-oomkill
    source: arc-runners/my-runner-abc123-runner-x7k9z
  pod:
    name: my-runner-abc123-runner-x7k9z
    namespace: arc-runners
    phase: Running
    nodeName: worker-node-1
    phaseHistory:
      - from: ""
        to: Pending
        timestamp: "2026-03-09T06:11:09Z"
      - from: Pending
        to: Running
        timestamp: "2026-03-09T06:11:30Z"
    containerStatuses:
      - name: runner
        state: terminated
        reason: OOMKilled
        exitCode: 137
        restartCount: 0
        oomKilled: true
    conditions:
      - type: Ready
        status: "False"
        reason: PodFailed
      - type: ContainersReady
        status: "False"
        reason: PodFailed
  timeline:
    - timestamp: "2026-03-09T06:11:38Z"
      source: pod
      type: pod-oomkill
      message: "Anomaly detected on pod my-runner-abc123-runner-x7k9z: pod-oomkill"
  diagnosis:
    failureType: pod-oomkilled
    summary: Runner pod OOMKilled (pod arc-runners/my-runner-abc123-runner-x7k9z)
    remediation: Increase memory limits on the runner pod template.
  logPaths:
    - arc-runners/my-runner-abc123-runner-x7k9z/runner/2026-03-09T06-11-38.log
status:
  phase: Complete
```

### Acting on an investigation

Once you see a completed investigation, the `spec.diagnosis` tells you what happened and what to do:

**1. Read the diagnosis**

```sh
kubectl get investigation pod-my-runner-abc123-runner-x7k9z -n arc-runners \
  -o jsonpath='{.spec.diagnosis}' | jq .
```

```json
{
  "failureType": "pod-oomkilled",
  "summary": "Runner pod OOMKilled (pod arc-runners/my-runner-abc123-runner-x7k9z)",
  "remediation": "Increase memory limits on the runner pod template."
}
```

**2. Fix the root cause** based on the failure type:

- **`pod-oomkilled`** — Update your ARC runner scale set Helm values to increase `template.spec.containers[0].resources.limits.memory`. If only specific workflows need more memory, consider a dedicated runner scale set with higher limits for those jobs.

- **`pod-crashloop`** — Check the collected logs at `spec.logPaths` for the crash reason. Common causes: broken post-job hooks, missing binaries in the runner image, or misconfigured entrypoint scripts.

- **`pod-stuck-terminating`** — The pod's finalizers are preventing cleanup. Check `spec.pod.conditions` and `spec.pod.events` for the cause. Usually a node issue or a stuck volume unmount. Force-delete with `kubectl delete pod <name> --grace-period=0 --force` if needed.

- **`runner-stuck-running`** or **`runner-stuck-failed`** — Delete the stuck EphemeralRunner CR: `kubectl delete ephemeralrunner <name> -n <namespace>`. Check the ARC controller logs in `arc-systems` for why cleanup didn't happen automatically.

- **`job-stuck-queued`** — No runner is available to pick up the job. Check that the listener pod is running (`kubectl get pods -n arc-systems`) and that the AutoScalingRunnerSet's `maxRunners` isn't already reached.

**3. Check collected logs** (if log storage is configured):

The controller collects container logs from failed pods before they're deleted. Log paths are listed in `spec.logPaths`. If you configured a PVC, the logs are stored at the mount path (default `/var/log/arc-detective/`):

```sh
# Exec into the controller pod to read collected logs
kubectl exec -n arc-detective-system deployment/arc-detective-controller-manager -- \
  cat /var/log/arc-detective/arc-runners/my-runner-abc123-runner-x7k9z/runner/2026-03-09T06-11-38.log
```

**4. Clean up** — Completed investigations are automatically deleted after the `retentionPeriod` (default 7 days). To delete one manually:

```sh
kubectl delete investigation pod-my-runner-abc123-runner-x7k9z -n arc-runners
```

### Uninstall

```sh
make undeploy
make uninstall
```

## Development

### Running tests

```sh
# Unit and integration tests (no cluster needed)
make test

# Basic e2e test (creates a local Kind cluster, deploys the operator, verifies it starts)
make test-e2e

# Full e2e test (requires a real GitHub repo with ARC runners)
# See test/e2e-full/ for details on required environment variables and PAT permissions
export GITHUB_TOKEN="ghp_..."
export ARC_DETECTIVE_TEST_REPO="myorg/my-test-repo"
make test-e2e-full
```

### Project layout

```
api/v1alpha1/          # CRD types (DetectiveConfig, Investigation)
cmd/                   # Manager entrypoint
internal/controller/   # Controllers (PodWatcher, Correlator, EphemeralRunnerWatcher, GitHubPoller, Cleanup)
internal/diagnosis/    # Failure pattern matching
internal/github/       # GitHub API client
internal/logcollector/ # Pod log collection
config/                # Kustomize manifests (CRDs, RBAC, deployment)
test/e2e/              # Basic e2e tests
test/e2e-full/         # Full e2e tests against real GitHub Actions
test/integration/      # Integration tests (envtest with mock GitHub)
```

## License


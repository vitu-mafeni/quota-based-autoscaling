# quota-based-autoscaling
## SIMULATION GOAL

We want to achieve the following:
1. Phase 1 → Quota Scaling
- ResourceQuota starts small.
- A Deployment tries to use more CPU/memory.
- The controller increases the quota.
2. Phase 2 → Node Scaling
- Quota reaches max limit.
- Pods cannot schedule because cluster has no free CPU. 
- The controller triggers node scaling.
This simulation will reproduce that exact behavior.

## Simulation Setup
### STEP A — Create a Small ResourceQuota (easy to exceed)
```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: quota-test
---
apiVersion: v1
kind: ResourceQuota
metadata:
  name: sample-appliedquota
  namespace: quota-test
spec:
  hard:
    limits.cpu: "500m"
    limits.memory: "512Mi"
    requests.cpu: "500m"
    requests.memory: "512Mi"
```
**Meaning**:
Only 0.5 CPU and 512Mi total namespace limit.
The controller will scale this up until:
- maxQuota.cpu
- maxQuota.memory
from the NamespaceQuota CRD.

### STEP B — Create NamespaceQuota CR to define scaling behavior
Use this (tuned for simulation):
```yaml
apiVersion: scaling.dcn.ssu.ac.kr/v1
kind: NamespaceQuota
metadata:
  name: quota-scaler
spec:
  appliedQuotaRef:
    namespace: quota-test
    name: sample-appliedquota

  behavior:
    quotaScaling:
      enabled: true
      minQuota:
        cpu: "500m"
        memory: "512Mi"
      maxQuota:
        cpu: "2"        # Stop scaling at 2 CPU
        memory: "3Gi"   # Stop scaling at 3Gi
      scaleStep:
        cpu: "500m"     # Each scaling step adds 0.5 cores
        memory: "512Mi" # Each scaling step adds 512Mi
      targetQuotaUtilization: 70

    nodeScaling:
      enabled: true
      minNodes: 1
      maxNodes: 4
      nodeFlavor: small
      scaleUpThreshold: 85
      scaleUpCooldownSeconds: 60
```
Scaling Logic Simulated Here:
- Quota increases in 0.5 CPU / 512Mi steps.
- Stops at 2 CPU / 3Gi.
- After quota maxed → nodes need to scale up.

### STEP C — Deployment that will push quota limits first
(This simulates gradually filling the quota)
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gradual-load
  namespace: quota-test
spec:
  replicas: 1
  selector:
    matchLabels:
      app: gradual-load
  template:
    metadata:
      labels:
        app: gradual-load
    spec:
      containers:
      - name: load
        image: busybox
        command: ["sh", "-c", "while true; do :; done"]
        resources:
          requests:
            cpu: "400m"
            memory: "300Mi"
          limits:
            cpu: "400m"
            memory: "300Mi"
```
**What Happens:**
1. First pod starts ⇒ quota usage hits ~80%
→ Quota Scaling Round #1 triggers.
2. Controller patches quota:
- CPU: 500m → 1000m
- Memory: 512Mi → 1024Mi
3. You increase replicas to 2
→ It pushes usage >70% again
→ Quota Scaling Round #2
Repeat until maxQuota.

### STEP D — Deployment that forces Node Scaling
When quota can no longer increase, apply this:
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: heavy-load
  namespace: quota-test
spec:
  replicas: 5
  selector:
    matchLabels:
      app: heavy-load
  template:
    metadata:
      labels:
        app: heavy-load
    spec:
      containers:
      - name: heavy
        image: nginx
        resources:
          requests:
            cpu: "500m"
            memory: "400Mi"
          limits:
            cpu: "500m"
            memory: "400Mi"
```
**Why it triggers Node Scaling:**
- Total requested CPU = 5 × 500m = 2.5 CPU
- But quota max = 2 CPU
- Even after quota maxed out:
    - cluster might not have enough allocatable CPU
- Pods remain pending
→ Thew= controller detects no free cluster resources
→ Node Scale-Up triggered

## installation steps

### 1. Create the gitea secret
```bash
kubectl create secret generic git-user-secret \
  --from-literal=username=nephio \
  --from-literal=password=secret \
  -n default
```
### 2. Create the following environment variables where you will run the controller
```bash
export GIT_SERVER_URL="http://47.129.115.173:31413"
export GIT_SECRET_NAME="git-user-secret"
export GIT_SECRET_NAMESPACE="default"
# On managament cluster, this variable has to be set
export SERVER_TYPE="MGMT"
```
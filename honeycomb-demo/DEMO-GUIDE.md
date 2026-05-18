# Honeycomb Demo — Step-by-Step Guide

> **App**: URL Shortener microservices (link-service, redirect-service, stats-service)
> **Cluster**: EKS `honeycomb-demo` — us-east-2 — 2 nodes
> **Goal**: Break a live service, get a Slack alert, and use Honeycomb to find the root cause in under 2 minutes

---

## PHASE 0 — One-Time Setup (do this before recording day)

### 0.1 — Install Tools

```bash
# AWS CLI
curl "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o "awscliv2.zip"
unzip awscliv2.zip && sudo ./aws/install

# eksctl
curl -sLO "https://github.com/eksctl-io/eksctl/releases/latest/download/eksctl_Linux_amd64.tar.gz"
tar -xzf eksctl_Linux_amd64.tar.gz && sudo mv eksctl /usr/local/bin

# kubectl
curl -LO "https://dl.k8s.io/release/$(curl -Ls https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
chmod +x kubectl && sudo mv kubectl /usr/local/bin

# Docker
sudo apt-get update && sudo apt-get install -y docker.io
sudo usermod -aG docker $USER && newgrp docker
docker login
```

### 0.2 — Configure AWS

```bash
aws configure
# Region: us-east-2

aws sts get-caller-identity   # verify
```

### 0.3 — Create EKS Cluster

```bash
eksctl create cluster \
  --name honeycomb-demo \
  --region us-east-2 \
  --nodegroup-name demo-nodes \
  --node-type t3.medium \
  --nodes 2 \
  --managed \
  --node-ami-family AmazonLinux2023
```

After the cluster is up, set up required addons:

#### EBS CSI Driver (postgres needs this for persistent storage)

```bash
# Associate OIDC provider
eksctl utils associate-iam-oidc-provider \
  --cluster honeycomb-demo --region us-east-2 --approve

# Create IRSA for EBS CSI (pin a role name so re-runs are idempotent)
eksctl create iamserviceaccount \
  --cluster honeycomb-demo --region us-east-2 \
  --namespace kube-system --name ebs-csi-controller-sa \
  --role-name AmazonEKS_EBS_CSI_DriverRole \
  --attach-policy-arn arn:aws:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy \
  --approve --override-existing-serviceaccounts

# Install the addon
eksctl create addon \
  --name aws-ebs-csi-driver \
  --cluster honeycomb-demo \
  --region us-east-2 \
  --force
```

> **If `eksctl create iamserviceaccount` fails with `AmazonEKS_EBS_CSI_DriverRole already exists`:**
> the role is left over from a prior cluster and its trust policy points at the old OIDC issuer.
> Repoint it at the current cluster, then annotate the SA manually:
>
> ```bash
> # Get current cluster's OIDC issuer
> OIDC=$(aws eks describe-cluster --name honeycomb-demo --region us-east-2 \
>   --query 'cluster.identity.oidc.issuer' --output text | sed 's|https://||')
> ACCOUNT=$(aws sts get-caller-identity --query Account --output text)
>
> # Rewrite the role's trust policy
> cat > /tmp/ebs-csi-trust.json <<EOF
> {
>   "Version": "2012-10-17",
>   "Statement": [{
>     "Effect": "Allow",
>     "Principal": {"Federated": "arn:aws:iam::${ACCOUNT}:oidc-provider/${OIDC}"},
>     "Action": "sts:AssumeRoleWithWebIdentity",
>     "Condition": {"StringEquals": {
>       "${OIDC}:aud": "sts.amazonaws.com",
>       "${OIDC}:sub": "system:serviceaccount:kube-system:ebs-csi-controller-sa"
>     }}
>   }]
> }
> EOF
> aws iam update-assume-role-policy --role-name AmazonEKS_EBS_CSI_DriverRole \
>   --policy-document file:///tmp/ebs-csi-trust.json
> aws iam attach-role-policy --role-name AmazonEKS_EBS_CSI_DriverRole \
>   --policy-arn arn:aws:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy
>
> # Delete the failed CloudFormation stack (disable termination protection first)
> STACK=eksctl-honeycomb-demo-addon-iamserviceaccount-kube-system-ebs-csi-controller-sa
> aws cloudformation update-termination-protection --stack-name $STACK \
>   --no-enable-termination-protection --region us-east-2
> aws cloudformation delete-stack --stack-name $STACK --region us-east-2
>
> # Annotate the SA and restart the controller
> kubectl annotate serviceaccount ebs-csi-controller-sa -n kube-system \
>   eks.amazonaws.com/role-arn=arn:aws:iam::${ACCOUNT}:role/AmazonEKS_EBS_CSI_DriverRole --overwrite
> kubectl rollout restart deployment/ebs-csi-controller -n kube-system
> ```

#### nginx Ingress Controller (app access via AWS LoadBalancer)

```bash
kubectl apply -f https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.10.1/deploy/static/provider/aws/deploy.yaml

kubectl rollout status deployment/ingress-nginx-controller -n ingress-nginx --timeout=120s

# Enable server-snippet annotations (required for API routing)
kubectl patch configmap ingress-nginx-controller -n ingress-nginx \
  --type merge -p '{"data":{"allow-snippet-annotations":"true"}}'

kubectl rollout restart deployment/ingress-nginx-controller -n ingress-nginx
kubectl rollout status deployment/ingress-nginx-controller -n ingress-nginx --timeout=60s
```

#### Verify cluster is ready

```bash
kubectl get nodes                          # 2 nodes, STATUS=Ready
kubectl get pods -n kube-system | grep ebs # ebs-csi pods Running
kubectl get svc ingress-nginx-controller -n ingress-nginx  # EXTERNAL-IP populated
```

### 0.4 — Create Honeycomb Account

1. Go to **honeycomb.io** → Sign up free (no credit card, 20M events/month)
2. Note your **Environment** name (default: `test`)

### 0.5 — Get Honeycomb API Key

1. **Team Settings** → **API Keys** → Request keys --> **Create API Key**
Note: Do not use Ingest keys
2. Name: `demo-key` — enable **Send Events** + **Create Datasets**
3. Copy the key (starts with `hcaik_`)

```bash
export HONEYCOMB_API_KEY=hcaik_XXXXXXXXXXXXXXXXXXXX
echo 'export HONEYCOMB_API_KEY=hcaik_XXXXXXXXXXXXXXXXXXXX' >> ~/.bashrc
```

### 0.6 — Set Up Slack Notification

This is what makes the demo compelling — you get paged like a real on-call engineer.

1. In Slack → **Apps** → search **Incoming WebHooks** → Add to Workspace
2. Choose channel `#incidents` → copy the Webhook URL
3. In Honeycomb → **Team Settings** → **Integrations** → **Add Integration**
   - **Provider:** Webhook
   - **Name:** `slack-incidents`
   - **Webhook URL:** paste your Slack Incoming Webhook URL
   - **Shared Secret:** leave blank
4. Under **Payload → Trigger Alerts Template**, replace the default with this Slack-formatted template:

```json
{
  "text": ":rotating_light: *Honeycomb Alert Fired*",
  "attachments": [
    {
      "color": "{{ if eq .Alert.Status `TRIGGERED` }}#ff0000{{ else }}#36a64f{{ end }}",
      "pretext": "{{ if eq .Alert.Status `TRIGGERED` }}:red_circle: A trigger has fired and needs attention.{{ else }}:large_green_circle: Alert has resolved.{{ end }}",
      "title": "{{ .Name }}",
      "title_link": "{{ .Result.URL }}",
      "text": "{{ .Description }}",
      "fields": [
        {
          "title": "Status",
          "value": "{{ if eq .Alert.Status `TRIGGERED` }}:fire: FIRING{{ else }}:white_check_mark: RESOLVED{{ end }}",
          "short": true
        },
        {
          "title": "Environment",
          "value": "{{ .Environment }}",
          "short": true
        },
        {
          "title": "Condition",
          "value": "{{ .Operator }} {{ .Threshold }}",
          "short": true
        },
        {
          "title": "Groups Triggered",
          "value": "{{ len .Result.GroupsTriggered }}",
          "short": true
        },
        {
          "title": "Summary",
          "value": "{{ .Alert.Summary }}",
          "short": false
        },
        {
          "title": ":mag: Investigate",
          "value": "{{ .Result.URL }}",
          "short": false
        }
      ]
    }
  ]
}
```

5. Click **Preview** to validate the template parses without errors
6. Click **Add**

> **Test the format:** After saving, go to any trigger → **...** menu → **Send Test Notification**.
> You'll get a sample Slack message with fake data so you can verify the card looks right
> before the demo. The alert title will be a clickable link to Honeycomb.

> **Both triggers use this same integration.** When creating each trigger (Steps 0.10),
> set the recipient to this `slack-incidents` webhook — you don't need to configure
> the template again.

### 0.7 — Set Up Honeycomb MCP (for the IDE segment)

```bash
claude mcp add honeycomb --transport http https://mcp.honeycomb.io/mcp
# Opens browser → log in to Honeycomb → authorize

claude mcp list  # verify: honeycomb Connected

claude "list my Honeycomb datasets"  
```

### 0.8 — Deploy the App

```bash
cd ~/repos/testing_zone/honeycomb-demo
HONEYCOMB_API_KEY=hcaik_XXXXXXXXXXXXXXXXXXXX ./scripts/apply.sh
```

This clones the upstream URL shortener, patches all 3 services with OTel instrumentation,
builds Docker images, pushes to Docker Hub, and deploys to EKS.

Wait for `✅ Stack is live!` — takes ~5 minutes.

### 0.9 — Verify Traces Are Flowing

```bash
export APP_URL="http://$(kubectl get svc ingress-nginx-controller -n ingress-nginx \
  -o jsonpath='{.status.loadBalancer.ingress[0].hostname}')"
echo $APP_URL

# Send a test request
curl -s -X PUT "$APP_URL/api/generate" \
  -H "Content-Type: application/json" \
  -d '{"long":"https://honeycomb.io"}' | jq .

# Health check
curl -s "$APP_URL/api/stats/health"
```

In Honeycomb → **Home** → you should see datasets `redirect-service`, `link-service`, `stats-service`.
Click `redirect-service` → **Recent Traces** → confirm spans are flowing.

**Wait for some time for traces.**

### 0.10 — Create a Trigger (free plan gives you exactly 2)

#### Trigger 1 — High Redirect Latency

1. In Honeycomb → click dataset **`redirect-service`** → left nav **Triggers** → **New Trigger**
2. **Name:** `High Redirect Latency`
3. The default query starts with `AVG(duration_ms)` — **change `AVG` to `P99`**
   > AVG hides tail latency — if 10% of requests are slow, AVG stays low and the trigger never fires.
   > P99 spikes immediately when chaos is injected and fires within the first evaluation window.
4. Keep the **`is_root`** WHERE filter (filters to entry-point spans only, avoids double-counting)
5. **Threshold:** `is above` `500` (ms)
6. **Evaluation window:** `5 minutes` — this is intentionally short so the alert fires fast on camera
   > Honeycomb evaluates the trigger every 5 minutes. After injecting chaos, wait up to
   > **5 minutes** for the Slack alert to fire. Don't panic if it doesn't fire instantly — it's
   > checking on a fixed schedule, not in real time.
7. **Recipients** → select `slack-incidents` (the webhook from Step 0.6)
8. Click **Save**


### 0.11 — Create a Board (Health Dashboard)

In Honeycomb → **Boards** → **New Board** → name it `URL Shortener Health`.

Add these 3 queries and pin them:

1. **`redirect-service`** — `HEATMAP(duration_ms)` — title: `Redirect Latency`
2. **`stats-service`** — `COUNT WHERE status_code = 2` — title: `Error Rate`
3. **`link-service`** — `COUNT` grouped by `http.route` — title: `Request Rate`

This is your "calm baseline" view at the start of the demo.

### 0.12 — Warm Up With Load Test

Run at least 30 minutes before recording so the board has a healthy baseline:

```bash
export APP_URL="http://$(kubectl get svc ingress-nginx-controller -n ingress-nginx \
  -o jsonpath='{.status.loadBalancer.ingress[0].hostname}')"

./scripts/load-test.sh $APP_URL 10
# Let this run — do NOT stop it before injecting chaos
```

After 5-10 minutes of baseline traffic, inject chaos **while the load test is still running**:

```bash
./scripts/inject-chaos.sh 800
# Keep load test running — wait for Slack alert to fire (~1-2 min)
./scripts/remove-chaos.sh
# Keep load test running — watch latency recover in Honeycomb
# Ctrl+C the load test only after you're done
```

> **Important:** Never stop the load test before injecting or removing chaos.
> Triggers need live traffic to evaluate — no traffic means no alert.
> BubbleUp needs both slow AND fast spans to compare — that comparison
> only works when traffic is continuously flowing through the chaos.

---



### Canvas AI  🤖

In Honeycomb left nav → **Canvas** → new canvas:

Type:
```
Why did redirect latency spike? What's causing the slowdown?
```

Watch Canvas surface `chaos.active` and `chaos.latency_ms`.

> "Same answer. Whether you click through the UI or talk to the AI, you get there fast."

---

### PART 7 — MCP in Claude Code (1.5 min) 💻

Switch to terminal:

```bash
claude
```

In Claude Code chat:
```
Investigate the redirect-service in Honeycomb — why is latency high right now?
```

> It runs the queries, reads the traces, and tells me the root cause — right here."

Watch Claude query Honeycomb via MCP and return the answer with `chaos.latency_ms = 800`.

---

###  Fix It & Show Recovery  ✅

```bash
./scripts/remove-chaos.sh
```

Switch to board → watch P99 drop back to normal.

> "Feature flag off. Latency back to normal. The board is green again.
> Start to finish — alert, investigation, fix — under 5 minutes."

---

## Teardown

```bash
# Stop load test (Ctrl+C)

# Delete EKS cluster — stops all AWS charges
eksctl delete cluster --name honeycomb-demo --region us-east-2
```

**Cost while running:** 2 × t3.medium + EKS control plane ≈ **$0.18/hr** — delete same day.

---

## Troubleshooting

| Problem | Fix |
|---|---|
| `kubectl get nodes` shows nothing | `aws eks update-kubeconfig --name honeycomb-demo --region us-east-2` |
| Pods stuck `Pending` | `kubectl describe pod <name> -n url-shortener` — check Events section |
| postgres stuck `Pending` | PVC can't bind. Check `kubectl get pods -n kube-system \| grep ebs-csi-controller` — if it's CrashLoopBackOff with `UnauthorizedOperation` errors, IRSA is broken. Re-run step 0.3 EBS CSI setup (and follow the "role already exists" callout if `eksctl` errors). |
| No traces in Honeycomb | `kubectl logs -l app=otel-collector -n url-shortener --tail=50` — look for auth errors |
| Slack alert not firing | Confirm trigger is Active and load-test is running with enough traffic |
| Canvas not visible | Team Settings → Features → check if Canvas / Honeycomb Intelligence can be enabled |
| MCP not working | `claude mcp list` — verify honeycomb shows Connected; re-auth if expired |
| `apply.sh` fails on `docker push` | `docker login` first; set `export REGISTRY=yourdockerhubusername` |
| App URL not resolving | Re-export: `export APP_URL="http://$(kubectl get svc ingress-nginx-controller -n ingress-nginx -o jsonpath='{.status.loadBalancer.ingress[0].hostname}')"` |

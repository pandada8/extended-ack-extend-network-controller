## Pod NAT EIP Controller

This controller watches Pods with the following annotations:

```yaml
network-x.alibabacloud-x.com/pod-nat-eip: "1.2.3.4"
network-x.alibabacloud-x.com/pod-nat-gateway-id: "ngw-xxxx"
```

After the Pod gets a `status.podIP`, the controller ensures an Alibaba Cloud NAT Gateway SNAT rule exists for:

- `sourceCIDR=<podIP>/32`
- `snatIp=<annotation eip>`
- `natGatewayId=<annotation nat gateway id>`

The controller also updates this Pod condition:

```yaml
network-x.alibabacloud-x.com/NATGatewayEgressReady
```

After the rule is created or discovered, the controller writes these annotations back to the Pod:

```yaml
network-x.alibabacloud-x.com/pod-nat-snat-entry-id: "snat-xxxx"
network-x.alibabacloud-x.com/pod-nat-snat-table-id: "stb-xxxx"
network-x.alibabacloud-x.com/pod-nat-source-cidr: "10.0.0.12/32"
network-x.alibabacloud-x.com/pod-nat-last-request-id: "2315DEB7-..."
```

If the Pod template declares it as a `readinessGate`, the Pod only becomes Ready after the SNAT rule is available.

## Example Pod

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: demo
  annotations:
    network-x.alibabacloud-x.com/pod-nat-eip: "1.2.3.4"
    network-x.alibabacloud-x.com/pod-nat-gateway-id: "ngw-xxxx"
spec:
  readinessGates:
  - conditionType: network-x.alibabacloud-x.com/NATGatewayEgressReady
  containers:
  - name: app
    image: busybox:1.36
    command: ["sh", "-c", "sleep 3600"]
```

## Environment Variables

- `ALIBABA_CLOUD_REGION_ID`
- `ALIBABA_CLOUD_ACCESS_KEY_ID`
- `ALIBABA_CLOUD_ACCESS_KEY_SECRET`
- `ALIBABA_CLOUD_SECURITY_TOKEN` (optional)

## Container Image

The repository includes a `Dockerfile` for the controller image.

The GitHub Actions workflow `.github/workflows/publish-image.yaml` publishes images to:

```text
ghcr.io/<github-owner>/extended-ack-exteneded-network-controller
```

Published tags include:

- branch name
- git tag, such as `v0.1.0`
- commit SHA
- `latest` on the default branch

## Notes

- The controller adds a finalizer and removes the SNAT rule when the Pod is deleted.
- The controller expects Alibaba Cloud NAT Gateway to support `SourceCIDR=<podIP>/32`.
- This implementation treats an existing rule with the same source CIDR but a different EIP as an error.

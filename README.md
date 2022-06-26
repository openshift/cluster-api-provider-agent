# cluster-api-provider-agent
bla bla
Kubernetes-native declarative infrastructure for agent-based installation.

cluster-api-provider-agent serves as infrastructure provider for [Kubernetes cluster-api](https://github.com/kubernetes-sigs/cluster-api)

## How to install cluster-api-provider-agent

cluster-api-provider-agent is deployed into an existing OpenShift / Kubernetes cluster.

**Prerequisites:**
* Admin access to the OpenShift / Kubernetes cluster specified by the KUBECONFIG environment variable

### Installing with [clusterctl](https://cluster-api.sigs.k8s.io/clusterctl/overview.html)
Add the agent provider to [clusterctl configuration file](https://cluster-api.sigs.k8s.io/clusterctl/configuration.html) (located at `$HOME/.cluster-api/clusterctl.yaml`).
```yaml
providers:
  - name: "agent"
    url: "https://github.com/openshift/cluster-api-provider-agent/releases/latest/infrastructure-components.yaml"
    type: "InfrastructureProvider"
```
Set up the provider:
```shell
clusterctl init --infrastructure agent
```

### Installing from source
Build the cluster-api-provider-agent image and push it to a container image repository:
```shell
make docker-build docker-push IMG=<your docker repository>:`git log -1 --short`
```

Deploy cluster-api-provider-agent to your cluster:
```shell
make deploy IMG=<your docker repository>:`git log -1 --short`
```

### Installing namespace-scoped cluster-api-provider-agent
You can configure the provider to watch and manage AgentClusters and AgentMachines in a single Namespace.
You can also configure the provider to watch and use Agents from a different namespace.
Build the cluster-api-provider-agent image and push it to a container image repository:

Deploy cluster-api-provider-agent to your cluster:

```shell
make deploy IMG=<your docker repository>:`git log -1 --short` WATCH_NAMESPACE=foo AGENTS_NAMESPACE=bar
```
In case you are using namespace-scoped provider you will need to create Role-Based Access Control (RBAC) permissions
to give the provider permissions to access the resources see example [here](docs/namespaced-operator-permissions.md)

## Design
cluster-api-provider-agent utilizes the [Infrastructure Operator](https://github.com/openshift/assisted-service) for adjusting the amount of workers in an OpenShift cluster. The CRDs that it manages are:
 * AgentCluster
 * AgentMachine
 * AgentMachineTemplate

## High-level flow
 1. Using the Infrastructure Operator, create an InfraEnv suitable for your environment.
 1. Download the Discovery ISO from the InfraEnv's download URL and use it to boot one or more hosts. Each booted host will automatically run an agent process which will create an Agent CR in the InfraEnv's namespace.
 1. Approve Agents that you recognize and set any necessary properties (e.g., hostname, installation disk).
 1. When a new Machine is created, the CAPI provider will find an available Agent to associate with the Machine, and trigger its installation via the Infrastructure Operator.

## Drawbacks
### ProviderID controller
In order to get the [cluster machine approver](https://github.com/openshift/cluster-machine-approver) to approve the *kubelet-serving* CSRs
the cluster-api-provider-agent implements a provider-id controller that set the ProviderID from the appropriate Machine and add it to the Node's Spec.
This controller is a workaround in order to allow hands free e2e add hosts, it should be replaced by similar logic in the [Infrastructure Operator](https://github.com/openshift/assisted-service)

## Status
Until now this CAPI provider has been tested for resizing [HyperShift](https://github.com/openshift/hypershift) Hosted Clusters. It also has the following limitations:
 * The re-provisioning flow currently requires manually rebooting the host with the Discovery ISO.
 * The CAPI provider does not yet have cluster lifecycle features - it adds and removes nodes from an existing cluster.
 * The CAPI provider currently selects the first free Agent that is approved and whose validations are passing. It will be smarter in the future.


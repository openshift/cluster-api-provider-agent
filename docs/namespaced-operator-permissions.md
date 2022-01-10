### Namespace-scoped cluster-api-provider-agent roles and permissions
In case you choose to restrict the scope of the cluster-api-provider-agent to a specific Namespace, 
Role-Based Access Control (RBAC) permissions applied to the operator’s service account should be restricted accordingly.
These permissions are found in the directory config/rbac/. The ClusterRole in role.yaml and ClusterRoleBinding in role_binding.yaml 
are used to grant the provider permissions to access and manage its resources.

NOTE For changing the provider scope only the role.yaml and role_binding.yaml manifests need to be updated. 
To change the scope of the RBAC permissions from cluster-wide to a specific namespace, you will need to:
* Use Roles instead of ClusterRoles.
* Use RoleBindings instead of ClusterRoleBinding

Example Role-Based Access Control (RBAC) permissions 

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: capi-provider
  namespace: <watch namespace>
rules:
- apiGroups:
  - ""
  resources:
  - events
  - secrets
  - configmaps
  verbs:
  - '*'
- apiGroups:
  - bootstrap.cluster.x-k8s.io
  - controlplane.cluster.x-k8s.io
  - infrastructure.cluster.x-k8s.io
  - machines.cluster.x-k8s.io
  - exp.infrastructure.cluster.x-k8s.io
  - addons.cluster.x-k8s.io
  - exp.cluster.x-k8s.io
  - cluster.x-k8s.io
  resources:
  - '*'
  verbs:
  - '*'
- apiGroups:
  - hypershift.openshift.io
  resources:
  - '*'
  verbs:
  - '*'
- apiGroups:
  - coordination.k8s.io
  resources:
  - leases
  verbs:
  - '*'
- apiGroups:
  - extensions.hive.openshift.io
  resources:
  - agentclusterinstalls
  verbs:
  - '*'
- apiGroups:
  - capi-provider.agent-install.openshift.io
  resources:
  - '*'
  verbs:
  - '*'
- apiGroups:
  - hive.openshift.io
  resources:
  - clusterdeployments
  verbs:
  - '*'
```

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: capi-provider
  namespace: <watch namespace>
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: capi-provider
subjects:
- kind: ServiceAccount
  name: capi-provider
  namespace: <watch namespace>
```

Role-Based Access Control (RBAC) permissions for watching Agents in another namespace

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: agent-cluster-api
  namespace: <agents namespace>
rules:
- apiGroups:
  - agent-install.openshift.io
  resources:
  - agents
  verbs:
  - '*'
```

```yaml

apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: cluster-api
  namespace: <agents namespace>
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: agent-cluster-api
subjects:
- kind: ServiceAccount
  name: capi-provider
  namespace: clusters-test-infra-cluster-6faa3a95
```
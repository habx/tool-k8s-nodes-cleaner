# Why

This tool softly cordons and drains Kubernetes nodes for you. It avoids:
- Setting a replica=2 and a [PDB](https://kubernetes.io/docs/concepts/workloads/pods/disruptions/) for all deployments 
  (which is expensive and not always possible) and stateful sets.
- Manually terminating pods with ReadWriteOnce volumes (which requires human intervention)

# How to use it

```
Usage of ./k8s-nodes-cleaner:
  -confirm
        Request to confirm before applying changes
  -delete-nodes
        Delete after cleaning them
  -delete-pods-with-pv
        Delete pods that will most probably stay stuck
  -node-label value
        Node target label
  -node-version string
        Kubelet version to target
  -patch-id string
        Patch ID (default "2022-12-12")
```

To remove all nodes that have the `kill=me` label set, you can execute the following command:
```bash
k8s-nodes-cleaner --node-target-label "kill=me" -delete-pods-with-pv
```

To remove all nodes that have the `v1.19.13-eks-8df270` kubelet version:
```bash
k8s-nodes-cleaner --node-target-version v1.19.13-eks-8df270 -delete-pods-with-pv
```

# How it works

The general idea is to make it as smooth - like non-modifying and progressive - as possible for your cluster state.

This tool relies on a cluster auto-scaler to scale up the cluster with new nodes.

It:
- Cordons one node at a time based on some criteria (you can also cordon nodes manually)
- Lists all the pods on the currently cordoned node
- Updates the replicaset & daemonset of all the pods listed
- Optionally, can delete pods that have one volume with `ReadWriteOnce` access mode (with the `-delete-pods-with-pv` flag)
- Continues with the next node until it has processed all of them

# Known issue
## Could get stuck
It applies a conservative / non-destructive logic and thus could stay stuck forever. There is, for example,
no limit to how much time it waits for a node to lose all its pods.

## Is inefficient
In the cluster upgrade process, many pods of the same deployment move to different nodes. To avoid this limitation,
we would probably need to cordon more than 1 node at once.

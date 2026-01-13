package virtualkubelet

const (
	// ProvisionPVCOnAllEdgesAnnotations by default a PVC with the Liqo storage class is bound to the virtual node,
	// where the pod is scheduled. If this annotation is set to "true", the PVC can be provisioned on all edge nodes.
	// WARNING: each edge node will maintain its own version of the PVC. This is useful for avoiding
	// strict binding between a PVC and a specific cluster ID.
	ProvisionPVCOnAllEdgesAnnotations = "omni.cast.ai/provision-on-all-edge-nodes"
)

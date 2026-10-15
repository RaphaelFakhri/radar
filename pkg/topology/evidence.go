package topology

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/skyhook-io/radar/pkg/karpenter"
	"github.com/skyhook-io/radar/pkg/resourceid"
)

// edgeEvidence records why a manages edge exists. One edge type carries
// several kinds of relationship — Kubernetes ownership, GitOps management, a
// controller's spec or status link, a class binding, a display shortcut — and
// each answers different questions: ownership decides garbage collection,
// membership decides who deploys an object, a shortcut decides nothing.
// Producers record the evidence they used; a pair two producers both link
// carries both. The field is private and never serialized.
type edgeEvidence uint16

const (
	// evidenceOwner: a recorded ownerReference on the target names the source.
	evidenceOwner edgeEvidence = 1 << iota
	// evidenceController: that ownerReference is the target's controller.
	evidenceController
	// evidenceAssociation: the source's controller creates or binds the target
	// through its spec or status, or a name or label convention; no
	// ownerReference was read.
	evidenceAssociation
	// evidenceBinding: the target selects the source as its class.
	evidenceBinding
	// evidenceMembership: the source's controller recorded the target in its
	// status (Argo CD status.resources, a Flux inventory).
	evidenceMembership
	// evidenceOriginLabels: the target's Flux origin labels name the source by
	// name and namespace.
	evidenceOriginLabels
	// evidencePartialOriginLabel: the target's Flux name label names the source
	// and its namespace label is absent.
	evidencePartialOriginLabel
	// evidenceInstanceLabel: only the target's app.kubernetes.io/instance label
	// equals the source's name.
	evidenceInstanceLabel
	// evidenceShortcut: a display path across a hidden intermediate kind.
	evidenceShortcut
	// evidenceAggregate: the target is a synthetic group of objects.
	evidenceAggregate
)

// noEvidence marks an edge type that is not manages, whose meaning the type
// already says.
const noEvidence edgeEvidence = 0

// ownerEvidence is the evidence an ownerReference gives.
func ownerEvidence(ref metav1.OwnerReference) edgeEvidence {
	if ref.Controller != nil && *ref.Controller {
		return evidenceOwner | evidenceController
	}
	return evidenceOwner
}

// aggregateEvidence adds evidenceAggregate when the target is a synthetic
// Pod group rather than one object.
func aggregateEvidence(targetID string, evidence edgeEvidence) edgeEvidence {
	if strings.HasPrefix(targetID, "podgroup-") {
		return evidence | evidenceAggregate
	}
	return evidence
}

// helmReleaseLabelEvidence is how a target's labels match a Flux HelmRelease,
// or noEvidence when they do not.
func helmReleaseLabelEvidence(labels map[string]string, hrName, hrNamespace string) edgeEvidence {
	fluxName := labels[fluxHelmNameLabel]
	fluxNS := labels[fluxHelmNSLabel]
	switch {
	case fluxName == hrName && fluxNS == hrNamespace:
		return evidenceOriginLabels
	case fluxName == hrName && fluxNS == "":
		return evidencePartialOriginLabel
	case labels["app.kubernetes.io/instance"] == hrName:
		return evidenceInstanceLabel
	}
	return noEvidence
}

// nodePoolClaimEvidence is the evidence for the NodePool a resolver found for
// a NodeClaim through source.
func nodePoolClaimEvidence(claim metav1.Object, poolName string, source karpenter.RelationshipSource) edgeEvidence {
	if source == karpenter.RelationshipOwnerRef {
		for _, ref := range claim.GetOwnerReferences() {
			if ref.Kind == karpenter.NodePoolKind && ref.Name == poolName && resourceid.GroupFromAPIVersion(ref.APIVersion) == karpenter.Group {
				return ownerEvidence(ref)
			}
		}
	}
	return evidenceAssociation
}

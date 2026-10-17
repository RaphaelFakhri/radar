package issues

import (
	"github.com/skyhook-io/radar/internal/k8s"
	"github.com/skyhook-io/radar/pkg/issuesapi"
	"github.com/skyhook-io/radar/pkg/k8score"
)

// missingRefTargetResources are the kinds the missing-reference detectors
// look objects up in.
var missingRefTargetResources = []string{
	k8score.Secrets,
	k8score.ConfigMaps,
	k8score.PersistentVolumeClaims,
	k8score.ServiceAccounts,
	k8score.Services,
	k8score.Deployments,
	k8score.StatefulSets,
	k8score.DaemonSets,
	k8score.IngressClasses,
	k8score.StorageClasses,
	k8score.Roles,
	k8score.ClusterRoles,
}

// MissingRefUncheckedSources lists the reference-target kinds whose cache
// cannot confirm absence in namespaces (nil = every namespace) because Radar
// may list but not watch them. Missing references to those kinds are not
// reported, so an answer without them must not read as "no broken
// references".
func MissingRefUncheckedSources(cache *k8s.ResourceCache, namespaces []string) []issuesapi.UncheckedSource {
	if cache == nil || (namespaces != nil && len(namespaces) == 0) {
		return nil
	}
	scopes := namespaces
	if len(scopes) == 0 {
		scopes = []string{""}
	}
	var out []issuesapi.UncheckedSource
	for _, resource := range missingRefTargetResources {
		listOnly, stale := false, false
		for _, ns := range scopes {
			listOnly = listOnly || cache.KindListOnly(resource, ns)
			stale = stale || cache.KindStale(resource, ns)
		}
		if !listOnly {
			continue
		}
		reason := issuesapi.UncheckedReasonListOnly
		if stale {
			reason = issuesapi.UncheckedReasonStale
		}
		out = append(out, issuesapi.UncheckedSource{Source: issuesapi.UncheckedSourceMissingRef, Reason: reason, Resource: resource})
	}
	return out
}

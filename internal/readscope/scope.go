package readscope

// SelectNamespaces resolves a request selection within the allowed namespaces. nil `allowed`
// means the user is unrestricted; preserve `requested` (which may also be nil
// for cluster-wide). When the user is restricted, keep only the requested
// namespaces they're allowed to see; if `requested` is empty, fall back to
// the full allowed set. An empty requested selection means unspecified here;
// an empty allowed set means no access. Use NoNamespaceAccess before passing an
// authorization-filtered requested scope.
func SelectNamespaces(allowed, requested []string) []string {
	if allowed == nil {
		return requested
	}
	if len(requested) == 0 {
		return allowed
	}
	allowSet := make(map[string]struct{}, len(allowed))
	for _, ns := range allowed {
		allowSet[ns] = struct{}{}
	}
	out := make([]string, 0, len(requested))
	for _, ns := range requested {
		if _, ok := allowSet[ns]; ok {
			out = append(out, ns)
		}
	}
	return out
}

// NoNamespaceAccess returns true when a namespace filter explicitly grants no access
// (non-nil empty slice from auth filtering). Handlers with custom namespace logic
// should check this and return empty results.
func NoNamespaceAccess(namespaces []string) bool {
	return namespaces != nil && len(namespaces) == 0
}

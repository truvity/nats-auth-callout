package natsauthcallout

import (
	"fmt"
	"strings"
)

// Mapping-rule constants (v2).
const (
	// serviceAccountPrefix is how the TokenReview API reports
	// ServiceAccount identities.
	serviceAccountPrefix = "system:serviceaccount:"
	// employeeNamespacePrefix marks per-employee sandbox namespaces
	// (emp-{slug} — the roster's k8s abbreviation; the gitops tenants
	// stack renders both the namespace and its NATS account under this
	// name).
	employeeNamespacePrefix = "emp-"
	// ciNamespacePrefix marks CI tenant namespaces (ci-{org}-{repo},
	// one static namespace per CI-enabled repository).
	ciNamespacePrefix = "ci-"
	// legacyCINamespace is the pre-per-repo single CI namespace. Kept
	// until the layer-1 rows replace it, then deleted with this comment.
	legacyCINamespace = "ci"
)

// ParseServiceAccount splits an authenticated username of the form
// "system:serviceaccount:<namespace>:<name>" into its parts.
func ParseServiceAccount(username string) (namespace, name string, err error) {
	rest, ok := strings.CutPrefix(username, serviceAccountPrefix)
	if !ok {
		return "", "", fmt.Errorf("username %q is not a serviceaccount identity", username)
	}

	namespace, name, ok = strings.Cut(rest, ":")
	if !ok || namespace == "" || name == "" {
		return "", "", fmt.Errorf("malformed serviceaccount username %q", username)
	}

	return namespace, name, nil
}

// AccountForNamespace applies the v2 mapping rule — uniform across
// tenancy tiers, every tenant namespace maps to a DEDICATED account of
// the same name (broker accounts + tenants-stack Account CRs are
// rendered per namespace from the same cfg):
//   - namespace in projectAccounts                → account = namespace
//   - namespace "emp-{slug}" (non-empty)          → account = namespace
//   - namespace "ci-{org}-{repo}" (non-empty)     → account = namespace
//   - namespace exactly "ci" (legacy, transitional) → account = namespace
//   - anything else                               → rejected
func AccountForNamespace(namespace string, projectAccounts map[string]struct{}) (string, error) {
	if _, ok := projectAccounts[namespace]; ok {
		return namespace, nil
	}

	if slug, ok := strings.CutPrefix(namespace, employeeNamespacePrefix); ok && slug != "" {
		return namespace, nil
	}

	// A CI tenant namespace is ci-{org}-{repo}: BOTH components must be
	// present and non-empty. Accepting any non-empty suffix would map
	// arbitrary "ci-*" namespaces (e.g. a typo, or an unrelated
	// namespace someone names "ci-scratch") onto an account of their
	// own name. org and repo may themselves contain dashes, so the
	// rule is "at least two non-empty dash-separated components",
	// not an exact count.
	if suffix, ok := strings.CutPrefix(namespace, ciNamespacePrefix); ok {
		if org, repo, split := strings.Cut(suffix, "-"); split && org != "" && repo != "" {
			return namespace, nil
		}
	}

	if namespace == legacyCINamespace {
		return namespace, nil
	}

	return "", fmt.Errorf("namespace %q has no NATS account mapping", namespace)
}

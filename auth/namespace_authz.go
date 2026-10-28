package auth

import (
	"errors"

	"github.com/graybeardcorp/weft/store"
)

// ErrNamespaceForbidden is AuthorizeNamespaceTokens' refusal. The REST
// handlers answer it with 403 and the gRPC service with PermissionDenied.
var ErrNamespaceForbidden = errors.New("no permission to manage tokens and sources in this namespace")

// AuthorizeNamespaceTokens is the one rule for changing what feeds a namespace:
// creating its tokens and adding, removing or reconfiguring its sources.
// Owners pass. Anyone else needs the namespace's token permission (the
// <namespace>+tokens group). A namespace the caller can't see reads the same
// as one they may not touch, so the answer never says which exists.
//
// If we ever split source permissions from token permissions we'd add a
// dedicated flag; for now the two travel together (both register data
// ingestion), so reusing the one is honest.
func (s *Session) AuthorizeNamespaceTokens(st *store.Store, namespace string) error {
	if s.IsOwner {
		return nil
	}
	ns, err := st.GetNamespace(s.ActiveOrgID, namespace)
	if err != nil {
		return ErrNamespaceForbidden
	}
	perm, err := st.GetNamespacePermission(s.UserID, ns.ID)
	if err != nil || !perm.CanCreateTokens {
		return ErrNamespaceForbidden
	}
	return nil
}

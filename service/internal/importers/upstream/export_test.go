package upstream

import "github.com/sigstore/sigstore-go/pkg/root"

// TrustForTest makes s trust tm instead of the public-good root and require
// scts certificate-transparency timestamps: sigstore-go's virtual CA issues
// none.
func TrustForTest(s *Source, tm root.TrustedMaterial, scts int) {
	s.material = func() (root.TrustedMaterial, error) { return tm, nil }
	s.scts = scts
}

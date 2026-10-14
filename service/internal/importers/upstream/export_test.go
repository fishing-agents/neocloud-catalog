// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package upstream

import "github.com/sigstore/sigstore-go/pkg/root"

// TrustForTest trusts tm and requires scts CT timestamps; sigstore-go's virtual
// CA issues none.
func TrustForTest(s *Source, tm root.TrustedMaterial, scts int) {
	s.material = func() (root.TrustedMaterial, error) { return tm, nil }
	s.scts = scts
}

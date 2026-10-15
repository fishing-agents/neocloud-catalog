// SPDX-FileCopyrightText: 2026 Reza Jelveh
// SPDX-License-Identifier: Apache-2.0

package broker

import "time"

// SetNow sets the clock of the search cache.
func (b *Broker) SetNow(now func() time.Time) { b.now = now }

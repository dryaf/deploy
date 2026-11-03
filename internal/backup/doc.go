// Package backup coordinates archive storage, worker lifecycle, recovery, and
// durable notification state. These operations share transaction and lock-order
// invariants, so they remain in one package. Source capture, wire framing, helper
// installation, and provider delivery live in the source, protocol, helper, and
// notification subpackages respectively.
package backup

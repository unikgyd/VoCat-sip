package vowifi

// SIMReaderOptions is internal application policy, not a user-facing setting.
// Opt-in is safe only when paired with BuildEPDGAttempts and a live identity
// fence. Existing direct users of the strict reader APIs keep their behavior.
type SIMReaderOptions struct{ AllowUnresolvedMNCLength bool }

package struggle

// Compute labels and credits every minute in in.Window for one device,
// following docs/STRUGGLE-RULES.md at RulesVersion. It is deterministic:
// the same Input always yields the same Result.
//
// This is a placeholder that emits no minutes; the engine is implemented in
// build step 4 (docs/ARCHITECTURE.md).
func Compute(in Input) Result {
	_ = in
	return Result{RulesVersion: RulesVersion}
}

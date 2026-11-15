package main

import (
	"context"
	"encoding/json"
)

// Use positive integer limits, matching the argument values decoded from JSON.
func completenessLimit(v any) (int64, bool) {
	number, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	n, err := number.Int64()
	return n, err == nil && n > 0
}

// A declaration applies only after the response has passed target/expect checks.
// Ambiguous selectors and absent limits cannot prove completeness.
func declaredCompleteness(ctx context.Context, p Policy, value any, args map[string]any) Completeness {
	d := p.Completeness
	if d == nil {
		return "unknown"
	}
	switch d.Mode {
	case "single":
		return "true"
	case "limit":
		n, ok := completenessLimit(args[d.LimitArg])
		if !ok {
			return "unknown"
		}
		code, err := compile(d.Items)
		if err != nil {
			return "unknown"
		}
		values, err := evaluate(ctx, code, value)
		if err != nil || len(values) != 1 {
			return "unknown"
		}
		items, ok := values[0].([]any)
		if !ok {
			return "unknown"
		}
		if int64(len(items)) >= n {
			return "false"
		}
		return "true"
	}
	return "unknown"
}

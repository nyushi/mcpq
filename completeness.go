package main

import (
	"context"
	"encoding/json"
	"math"
	"math/big"
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
	case "signal":
		return signalCompleteness(ctx, d, value)
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

// signalValue accepts exactly one jq result, as in limit mode.
func signalValue(ctx context.Context, path string, value any) (any, bool) {
	code, err := compile(path)
	if err != nil {
		return nil, false
	}
	values, err := evaluate(ctx, code, value)
	if err != nil || len(values) != 1 {
		return nil, false
	}
	return values[0], true
}

func signalCompleteness(ctx context.Context, d *CompletenessPolicy, value any) Completeness {
	incomplete := false
	if d.HasMore != "" {
		v, valid := signalValue(ctx, d.HasMore, value)
		more, ok := v.(bool)
		if !valid || !ok {
			return "unknown"
		}
		incomplete = more
	}
	if d.Total != "" {
		v, valid := signalValue(ctx, d.Items, value)
		items, ok := v.([]any)
		if !valid || !ok {
			return "unknown"
		}
		v, valid = signalValue(ctx, d.Total, value)
		if !valid {
			return "unknown"
		}
		total := new(big.Int)
		switch n := v.(type) {
		case int:
			total.SetInt64(int64(n))
		case *big.Int:
			total.Set(n)
		case float64:
			if math.IsNaN(n) || math.IsInf(n, 0) {
				return "unknown"
			}
			if _, accuracy := new(big.Float).SetFloat64(n).Int(total); accuracy != big.Exact {
				return "unknown"
			}
		default:
			return "unknown"
		}
		switch big.NewInt(int64(len(items))).Cmp(total) {
		case 1:
			return "unknown"
		case -1:
			incomplete = true
		}
	}
	if incomplete {
		return "false"
	}
	return "true"
}

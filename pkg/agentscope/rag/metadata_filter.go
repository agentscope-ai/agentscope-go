package rag

import (
	"fmt"
	"math"
	"strings"
	"unicode"

	"github.com/qdrant/go-client/qdrant"
)

// MetadataFilter is a conjunction (AND) of typed conditions on top-level payload
// keys. Nil matches all documents. It is a query filter, not authorization.
type MetadataFilter []MetadataCondition

// MetadataCondition is constructed with MetadataEqualString, MetadataEqualBool,
// MetadataEqualInt, MetadataEqualFloat or MetadataInRange. Its zero value is
// invalid. Conditions own their values and can be reused between queries.
type MetadataCondition struct {
	key     string
	kind    byte
	text    string
	boolean bool
	integer int64
	number  float64
	bounds  [4]float64
	present [4]bool
}

// NumericRange describes optional exclusive (GT/LT) or inclusive (GTE/LTE)
// float64 bounds. At least one bound is required. Do not set both lower bounds
// or both upper bounds. Integer equality retains int64 precision; ranges have
// float64 precision. MetadataInRange copies the pointed-to values.
type NumericRange struct{ GT, GTE, LT, LTE *float64 }

// MetadataEqualString matches a keyword exactly (not a full-text match).
func MetadataEqualString(key, value string) MetadataCondition {
	return MetadataCondition{key: key, kind: 's', text: value}
}

// MetadataEqualBool matches a boolean, including false.
func MetadataEqualBool(key string, value bool) MetadataCondition {
	return MetadataCondition{key: key, kind: 'b', boolean: value}
}

// MetadataEqualInt matches an integer without conversion to float64.
func MetadataEqualInt(key string, value int64) MetadataCondition {
	return MetadataCondition{key: key, kind: 'i', integer: value}
}

// MetadataEqualFloat matches a finite float using an inclusive equal-endpoint range.
func MetadataEqualFloat(key string, value float64) MetadataCondition {
	return MetadataCondition{key: key, kind: 'f', number: value}
}

// MetadataInRange constructs a numeric range, copying its bounds. Invalid
// conditions are rejected by index construction or query before embedding/RPC.
func MetadataInRange(key string, r NumericRange) MetadataCondition {
	c := MetadataCondition{key: key, kind: 'r'}
	for i, p := range []*float64{r.GT, r.GTE, r.LT, r.LTE} {
		if p != nil {
			c.bounds[i] = *p
			c.present[i] = true
		}
	}
	return c
}

func (i *QdrantIndex) queryFilter(filter MetadataFilter) (*qdrant.Filter, error) {
	conditions := make([]*qdrant.Condition, 0, len(i.requiredFilter)+len(filter))
	for _, group := range []MetadataFilter{i.requiredFilter, filter} {
		for _, c := range group {
			condition, err := c.qdrantCondition(i.vectorMetaKey)
			if err != nil {
				return nil, err
			}
			conditions = append(conditions, condition)
		}
	}
	if len(conditions) == 0 {
		return nil, nil
	}
	return &qdrant.Filter{Must: conditions}, nil
}

func (c *MetadataCondition) qdrantCondition(vectorKey string) (*qdrant.Condition, error) {
	// Qdrant interprets dots/brackets as paths; this API only addresses literal
	// top-level metadata keys. Internal content/vector payloads are reserved.
	if c.key == "" || c.key == "content" || c.key == vectorKey || strings.ContainsAny(c.key, ".[]\\\"") || strings.IndexFunc(c.key, unicode.IsControl) >= 0 {
		return nil, fmt.Errorf("qdrant: invalid or reserved metadata key %q", c.key)
	}
	switch c.kind {
	case 's':
		return qdrant.NewMatchKeyword(c.key, c.text), nil
	case 'b':
		return qdrant.NewMatchBool(c.key, c.boolean), nil
	case 'i':
		return qdrant.NewMatchInt(c.key, c.integer), nil
	case 'f':
		if !finite(c.number) {
			return nil, fmt.Errorf("qdrant: metadata float must be finite")
		}
		return qdrant.NewRange(c.key, &qdrant.Range{Gte: &c.number, Lte: &c.number}), nil
	case 'r':
		if (!c.present[0] && !c.present[1] && !c.present[2] && !c.present[3]) || (c.present[0] && c.present[1]) || (c.present[2] && c.present[3]) {
			return nil, fmt.Errorf("qdrant: range needs nonconflicting bounds")
		}
		var bounds [4]*float64
		for j, has := range c.present {
			if has {
				if !finite(c.bounds[j]) {
					return nil, fmt.Errorf("qdrant: range bounds must be finite")
				}
				bounds[j] = &c.bounds[j]
			}
		}
		lower, upper := bounds[0], bounds[2]
		if lower == nil {
			lower = bounds[1]
		}
		if upper == nil {
			upper = bounds[3]
		}
		if lower != nil && upper != nil && (*lower > *upper || (*lower == *upper && (c.present[0] || c.present[2]))) {
			return nil, fmt.Errorf("qdrant: range is empty or reversed")
		}
		return qdrant.NewRange(c.key, &qdrant.Range{Gt: bounds[0], Gte: bounds[1], Lt: bounds[2], Lte: bounds[3]}), nil
	default:
		return nil, fmt.Errorf("qdrant: invalid metadata condition")
	}
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

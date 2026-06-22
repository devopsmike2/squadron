// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package query

import (
	"time"

	"github.com/google/uuid"
)

// Query represents any Squadron QL query
type Query interface {
	Accept(visitor QueryVisitor) (interface{}, error)
}

// QueryVisitor defines the visitor pattern for query AST traversal
type QueryVisitor interface {
	VisitTelemetryQuery(*TelemetryQuery) (interface{}, error)
	VisitBinaryOp(*BinaryOp) (interface{}, error)
	VisitFunctionCall(*FunctionCall) (interface{}, error)
	VisitAggregation(*Aggregation) (interface{}, error)
}

// TelemetryType represents the type of telemetry data
type TelemetryType string

const (
	TelemetryTypeMetrics TelemetryType = "metrics"
	TelemetryTypeLogs    TelemetryType = "logs"
	TelemetryTypeTraces  TelemetryType = "traces"
)

// TelemetryQuery represents a basic telemetry query
type TelemetryQuery struct {
	Type      TelemetryType
	Selectors map[string]*Selector
	Duration  time.Duration
	StartTime *time.Time
	EndTime   *time.Time
	Limit     int
}

// Accept implements Query interface
func (q *TelemetryQuery) Accept(visitor QueryVisitor) (interface{}, error) {
	return visitor.VisitTelemetryQuery(q)
}

// Selector represents a label selector with operator
type Selector struct {
	Label    string
	Operator SelectorOperator
	Value    string
}

// SelectorOperator represents the type of selector operation
type SelectorOperator string

const (
	SelectorOpEqual    SelectorOperator = "="
	SelectorOpNotEqual SelectorOperator = "!="
	SelectorOpRegex    SelectorOperator = "=~"
	SelectorOpNotRegex SelectorOperator = "!~"
)

// BinaryOp represents a binary operation between two queries
type BinaryOp struct {
	Left     Query
	Operator BinaryOperator
	Right    Query
}

// Accept implements Query interface
func (b *BinaryOp) Accept(visitor QueryVisitor) (interface{}, error) {
	return visitor.VisitBinaryOp(b)
}

// BinaryOperator represents binary operators
type BinaryOperator string

const (
	BinaryOpAdd      BinaryOperator = "+"
	BinaryOpSubtract BinaryOperator = "-"
	BinaryOpMultiply BinaryOperator = "*"
	BinaryOpDivide   BinaryOperator = "/"
	BinaryOpEqual    BinaryOperator = "=="
	BinaryOpNotEqual BinaryOperator = "!="
	BinaryOpLT       BinaryOperator = "<"
	BinaryOpGT       BinaryOperator = ">"
	BinaryOpLTE      BinaryOperator = "<="
	BinaryOpGTE      BinaryOperator = ">="
	BinaryOpAnd      BinaryOperator = "and"
	BinaryOpOr       BinaryOperator = "or"
)

// FunctionCall represents a function call on a query
type FunctionCall struct {
	Name string
	Args []Query
}

// Accept implements Query interface
func (f *FunctionCall) Accept(visitor QueryVisitor) (interface{}, error) {
	return visitor.VisitFunctionCall(f)
}

// Aggregation represents an aggregation with grouping
type Aggregation struct {
	Function string
	Query    Query
	By       []string // group by labels
}

// Accept implements Query interface
func (a *Aggregation) Accept(visitor QueryVisitor) (interface{}, error) {
	return visitor.VisitAggregation(a)
}

// QueryResult represents the result of a Squadron QL query
type QueryResult struct {
	Type      TelemetryType          `json:"type"`
	Timestamp time.Time              `json:"timestamp"`
	Labels    map[string]string      `json:"labels"`
	Value     interface{}            `json:"value"`
	Data      map[string]interface{} `json:"data,omitempty"`
}

// QueryMeta contains metadata about query execution.
//
// v0.89.25 (#641) — UsedRollups is always false in v0.89.x. The
// rollup-detection signal would come from a query planner that
// inspects the resolved storage tier per query; that planner is
// roadmap work, not built. Until then, the field is left wired so
// the JSON wire shape doesn't change between releases, but clients
// MUST NOT branch on the value — treat it as a placeholder. This
// is honest by way of code comment rather than wire-shape change:
// downstream API consumers that already parse the field continue
// to parse it; clients that don't read it don't need to.
type QueryMeta struct {
	ExecutionTime time.Duration `json:"execution_time"`
	RowCount      int           `json:"row_count"`
	QueryType     string        `json:"query_type"`
	// UsedRollups is a placeholder — see the type doc above. The
	// query planner that would compute this honestly is roadmap
	// work tracked as a (c)-class item in the v0.89.25 cleanup
	// inventory. Always false in v0.89.x.
	UsedRollups bool `json:"used_rollups"`
}

// ExecutionContext contains context for query execution
type ExecutionContext struct {
	StartTime *time.Time
	EndTime   *time.Time
	Limit     int
	AgentID   *uuid.UUID
	GroupID   *string
}

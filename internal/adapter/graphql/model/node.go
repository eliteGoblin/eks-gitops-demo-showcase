package model

// Node, Party and SearchResult are the Go types of the GraphQL interfaces
// and the union of the same names. They are empty on purpose: their members
// include domain entities (Participant, Plan, Provider), which must not carry
// GraphQL marker methods, so membership is the schema's business and the
// generated code decides it with a type switch.
type (
	Node         interface{}
	Party        interface{}
	SearchResult interface{}
)

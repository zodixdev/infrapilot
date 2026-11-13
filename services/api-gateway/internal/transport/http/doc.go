// Package http is the public REST edge (ADR-0003, ADR-0026): the generated OpenAPI strict server, credential
// handling, rate limiting, contract validation, edge pre-authorization and gRPC → problem+json mapping. It
// forwards to internal services and holds no business rules (ADR-0002).
package http

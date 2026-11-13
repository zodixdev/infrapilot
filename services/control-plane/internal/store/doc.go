// Package store holds the control-plane data access: sqlc-generated queries (package db) and the retention
// purge. Transactions and tenant context come from pkg/postgres (ADR-0018); migrations live in package
// migrations.
package store

// Package model defines Entente's internal, source-independent routing model.
//
// The layering borrows from the Kubernetes Gateway API: an Entrypoint is the
// attachment point (it maps 1:1 to an RDS route_config_name in the Envoy
// bootstrap), a Route says "which host, on which entrypoints, to which
// backend", and a Backend is a set of concrete endpoints. Security decisions
// belong to a separate Policy layer, which is not implemented yet; Routes only
// carry a reserved PolicyRef that must stay empty for now.
package model

import "net/netip"

// Source identifies where a Route or Backend came from.
type Source string

const (
	SourceDocker Source = "docker"
	SourceStatic Source = "static"
)

// Route attaches a host to one or more entrypoints and forwards all of its
// traffic (path prefix "/") to a single Backend.
type Route struct {
	// Name is unique across all sources, e.g. "docker/whoami" or "static/legacy".
	Name   string
	Source Source

	Entrypoints []string
	Host        string
	Backend     string

	// PolicyRef is reserved for the future Policy layer. A non-empty value is
	// rejected by the reconciler; silently ignoring it would fail open.
	PolicyRef string
}

// Backend is a named set of endpoints. It becomes one EDS Cluster in Envoy.
type Backend struct {
	// Name is unique across all sources and doubles as the Envoy cluster name.
	Name      string
	Source    Source
	Endpoints []netip.AddrPort
}

// Model is the validated result of a reconcile pass: everything in it is
// safe to translate into xDS resources.
type Model struct {
	Entrypoints []string
	Routes      []Route
	Backends    []Backend
}

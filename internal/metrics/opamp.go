// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package metrics

// OpAMPMetrics tracks metrics for the OpAMP server
type OpAMPMetrics struct {
	// Connection metrics
	AgentConnections      Gauge   `metric:"opamp_agent_connections" tags:"component=opamp" help:"Current number of connected agents"`
	AgentConnectionsTotal Counter `metric:"opamp_agent_connections_total" tags:"component=opamp" help:"Total number of agent connection attempts"`
	AgentDisconnectsTotal Counter `metric:"opamp_agent_disconnects_total" tags:"component=opamp" help:"Total number of agent disconnections"`
	ConnectionErrors      Counter `metric:"opamp_connection_errors_total" tags:"component=opamp" help:"Total number of connection errors"`

	// ADR 0042 — OpAMP channel authentication. These three split the connection
	// attempts by credential outcome so the rollout can watch the authenticated-
	// vs-unauthenticated ratio and know when every agent presents a token before
	// flipping opamp.require_auth on. AuthenticatedConnections: a valid
	// opamp:enroll bearer. UnauthenticatedConnections: accepted under grace with
	// no/invalid credential (the future-fatal path). RejectedConnections: refused
	// because enforcement is on and no valid credential was presented.
	AuthenticatedConnectionsTotal   Counter `metric:"opamp_authenticated_connections_total" tags:"component=opamp" help:"Total OpAMP connections accepted with a valid enrollment token"`
	UnauthenticatedConnectionsTotal Counter `metric:"opamp_unauthenticated_connections_total" tags:"component=opamp" help:"Total OpAMP connections accepted WITHOUT a valid token under grace mode"`
	RejectedConnectionsTotal        Counter `metric:"opamp_rejected_connections_total" tags:"component=opamp" help:"Total OpAMP connections rejected because auth is required and no valid token was presented"`
	OversizedMessagesTotal          Counter `metric:"opamp_oversized_messages_total" tags:"component=opamp" help:"Total OpAMP messages dropped for exceeding the max message size"`
	RateLimitedMessagesTotal        Counter `metric:"opamp_rate_limited_messages_total" tags:"component=opamp" help:"Total OpAMP messages dropped by the per-connection rate limit"`

	// Message metrics
	MessagesReceived       Counter `metric:"opamp_messages_received_total" tags:"component=opamp" help:"Total number of OpAMP messages received"`
	MessagesSent           Counter `metric:"opamp_messages_sent_total" tags:"component=opamp" help:"Total number of OpAMP messages sent"`
	MessageErrors          Counter `metric:"opamp_message_errors_total" tags:"component=opamp" help:"Total number of message processing errors"`
	MessageProcessDuration Timer   `metric:"opamp_message_process_duration_seconds" tags:"component=opamp" help:"OpAMP message processing duration in seconds"`

	// Agent status updates
	StatusUpdateReceived Counter `metric:"opamp_status_updates_received_total" tags:"component=opamp" help:"Total number of agent status updates received"`
	StatusUpdateErrors   Counter `metric:"opamp_status_update_errors_total" tags:"component=opamp" help:"Total number of status update processing errors"`

	// Configuration updates
	ConfigUpdatesSent  Counter `metric:"opamp_config_updates_sent_total" tags:"component=opamp" help:"Total number of config updates sent to agents"`
	ConfigUpdateErrors Counter `metric:"opamp_config_update_errors_total" tags:"component=opamp" help:"Total number of config update errors"`

	// Health reporting
	HealthReportReceived Counter `metric:"opamp_health_reports_received_total" tags:"component=opamp" help:"Total number of health reports received from agents"`
	HealthReportErrors   Counter `metric:"opamp_health_report_errors_total" tags:"component=opamp" help:"Total number of health report processing errors"`

	// Package management
	PackagesSent  Counter `metric:"opamp_packages_sent_total" tags:"component=opamp" help:"Total number of packages sent to agents"`
	PackageErrors Counter `metric:"opamp_package_errors_total" tags:"component=opamp" help:"Total number of package delivery errors"`

	// Server state
	ServerStartTime Gauge `metric:"opamp_server_start_time_seconds" tags:"component=opamp" help:"OpAMP server start time in Unix epoch seconds"`
}

// NewOpAMPMetrics creates and initializes OpAMP metrics
func NewOpAMPMetrics(factory Factory) *OpAMPMetrics {
	metrics := &OpAMPMetrics{}
	MustInit(metrics, factory, nil)
	return metrics
}

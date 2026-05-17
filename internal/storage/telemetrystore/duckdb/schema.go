package duckdb

// TelemetrySchema defines the DuckDB schema for telemetry data
const TelemetrySchema = `
-- Metrics tables
CREATE TABLE IF NOT EXISTS metrics_sum (
	timestamp TIMESTAMP NOT NULL,
	agent_id VARCHAR NOT NULL,
	group_id VARCHAR,
	group_name VARCHAR,
	service_name VARCHAR NOT NULL,
	metric_name VARCHAR NOT NULL,
	metric_description VARCHAR,
	value DOUBLE NOT NULL,
	resource_attributes JSON,
	metric_attributes JSON
);

CREATE TABLE IF NOT EXISTS metrics_gauge (
	timestamp TIMESTAMP NOT NULL,
	agent_id VARCHAR NOT NULL,
	group_id VARCHAR,
	group_name VARCHAR,
	service_name VARCHAR NOT NULL,
	metric_name VARCHAR NOT NULL,
	metric_description VARCHAR,
	value DOUBLE NOT NULL,
	resource_attributes JSON,
	metric_attributes JSON
);

CREATE TABLE IF NOT EXISTS metrics_histogram (
	timestamp TIMESTAMP NOT NULL,
	agent_id VARCHAR NOT NULL,
	group_id VARCHAR,
	group_name VARCHAR,
	service_name VARCHAR NOT NULL,
	metric_name VARCHAR NOT NULL,
	metric_description VARCHAR,
	count BIGINT NOT NULL,
	sum DOUBLE NOT NULL,
	min DOUBLE,
	max DOUBLE,
	bucket_counts BIGINT[],
	explicit_bounds DOUBLE[],
	resource_attributes JSON,
	metric_attributes JSON
);

-- Logs table
CREATE TABLE IF NOT EXISTS logs (
	timestamp TIMESTAMP NOT NULL,
	agent_id VARCHAR NOT NULL,
	group_id VARCHAR,
	group_name VARCHAR,
	service_name VARCHAR NOT NULL,
	severity_text VARCHAR,
	severity_number INTEGER,
	body VARCHAR,
	trace_id VARCHAR,
	span_id VARCHAR,
	resource_attributes JSON,
	log_attributes JSON
);

-- Traces table
CREATE TABLE IF NOT EXISTS traces (
	timestamp TIMESTAMP NOT NULL,
	agent_id VARCHAR NOT NULL,
	group_id VARCHAR,
	group_name VARCHAR,
	trace_id VARCHAR NOT NULL,
	span_id VARCHAR NOT NULL,
	parent_span_id VARCHAR,
	service_name VARCHAR NOT NULL,
	span_name VARCHAR NOT NULL,
	span_kind VARCHAR,
	duration BIGINT NOT NULL,
	status_code VARCHAR,
	status_message VARCHAR,
	resource_attributes JSON,
	span_attributes JSON,
	events JSON,
	links JSON
);

-- Per-batch ingest accounting, written once per inbound OTLP
-- ExportRequest. The receiver records the wire-size of each batch
-- here so the Cost Insights surfaces can answer "how many bytes did
-- agent X send in the last hour" without re-summing JSON columns
-- off the row tables. agent_id is the same identifier carried on
-- the spans/metrics/logs tables; signal_type is one of "traces" |
-- "metrics" | "logs". item_count records how many spans / data
-- points / log records were in the batch; dropped_count records
-- how many of those the worker pool refused to enqueue. payload_bytes
-- is the post-decompression protobuf payload size.
--
-- Added in v0.24 as the foundation for the read-only Telemetry
-- Volume Insights surface; the v0.25 recommendation engine reads
-- from this same table.
CREATE TABLE IF NOT EXISTS otlp_batches (
	timestamp TIMESTAMP NOT NULL,
	agent_id VARCHAR NOT NULL,
	signal_type VARCHAR NOT NULL,
	item_count BIGINT NOT NULL,
	dropped_count BIGINT NOT NULL DEFAULT 0,
	payload_bytes BIGINT NOT NULL,
	status VARCHAR NOT NULL DEFAULT 'ok'
);
CREATE INDEX IF NOT EXISTS idx_otlp_batches_agent_time ON otlp_batches(agent_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_otlp_batches_signal_time ON otlp_batches(signal_type, timestamp);

-- Rollup tables for pre-aggregated data
CREATE TABLE IF NOT EXISTS rollups_1m (
	window_start TIMESTAMP NOT NULL,
	agent_id VARCHAR,
	group_id VARCHAR,
	metric_name VARCHAR NOT NULL,
	count BIGINT NOT NULL,
	sum DOUBLE NOT NULL,
	avg DOUBLE NOT NULL,
	min DOUBLE NOT NULL,
	max DOUBLE NOT NULL,
	PRIMARY KEY (window_start, agent_id, group_id, metric_name)
);

CREATE TABLE IF NOT EXISTS rollups_5m (
	window_start TIMESTAMP NOT NULL,
	agent_id VARCHAR,
	group_id VARCHAR,
	metric_name VARCHAR NOT NULL,
	count BIGINT NOT NULL,
	sum DOUBLE NOT NULL,
	avg DOUBLE NOT NULL,
	min DOUBLE NOT NULL,
	max DOUBLE NOT NULL,
	PRIMARY KEY (window_start, agent_id, group_id, metric_name)
);

CREATE TABLE IF NOT EXISTS rollups_1h (
	window_start TIMESTAMP NOT NULL,
	agent_id VARCHAR,
	group_id VARCHAR,
	metric_name VARCHAR NOT NULL,
	count BIGINT NOT NULL,
	sum DOUBLE NOT NULL,
	avg DOUBLE NOT NULL,
	min DOUBLE NOT NULL,
	max DOUBLE NOT NULL,
	PRIMARY KEY (window_start, agent_id, group_id, metric_name)
);

CREATE TABLE IF NOT EXISTS rollups_1d (
	window_start TIMESTAMP NOT NULL,
	agent_id VARCHAR,
	group_id VARCHAR,
	metric_name VARCHAR NOT NULL,
	count BIGINT NOT NULL,
	sum DOUBLE NOT NULL,
	avg DOUBLE NOT NULL,
	min DOUBLE NOT NULL,
	max DOUBLE NOT NULL,
	PRIMARY KEY (window_start, agent_id, group_id, metric_name)
);
`

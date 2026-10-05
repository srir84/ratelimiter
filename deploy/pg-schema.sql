-- Harness hits table (DESIGN §C8). IF NOT EXISTS so `make setup` can rerun (plan choice).
CREATE TABLE IF NOT EXISTS hits (
  user_id text,
  request_id text,
  arrival_ns bigint NOT NULL,
  PRIMARY KEY (user_id, request_id)
);

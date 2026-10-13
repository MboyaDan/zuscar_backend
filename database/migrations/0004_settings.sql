-- Settings, tax rates and document counters (docs/schema-constraints.md section 2).
CREATE TABLE system_settings (
  key        TEXT PRIMARY KEY CHECK (key ~ '^[a-z][a-z0-9_]*$'),
  value      JSONB       NOT NULL,
  updated_by BIGINT REFERENCES users (id) ON DELETE RESTRICT,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE tax_rates (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  code       TEXT        NOT NULL CHECK (code ~ '^[a-z][a-z0-9_]*$'),
  name       TEXT        NOT NULL,
  rate_bp    INTEGER     NOT NULL CHECK (rate_bp BETWEEN 0 AND 10000), -- basis points: 1600 = 16.00%
  active     BOOLEAN     NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT tax_rates_code_key UNIQUE (code)
);

-- Gapless document numbers: the row is locked FOR UPDATE inside the business transaction (A-05).
CREATE TABLE document_counters (
  doc_type   TEXT        NOT NULL CHECK (doc_type ~ '^[a-z][a-z0-9_]*$'),
  scope      TEXT        NOT NULL DEFAULT '', -- '' for continuous sequences; business date for kitchen tickets (S-05)
  prefix     TEXT        NOT NULL DEFAULT '',
  next_value BIGINT      NOT NULL CHECK (next_value >= 1),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (doc_type, scope)
);

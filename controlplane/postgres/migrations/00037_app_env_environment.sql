-- +goose Up
-- environment puts the environment INTO the config store's key (ADR-0028). app_env was keyed
-- (app, key) with no environment column, so an app had exactly one set of config values and a
-- deploy rendered that one set into whichever environment it targeted. Config was the one part of
-- an app that could not differ between staging and production: an app needing a different API
-- endpoint, log level or feature flag per environment had nowhere to say so, and a value set while
-- trying something in staging reached production on that app's next deploy there.
--
-- Every row now names exactly one environment. There is deliberately NO wildcard scope and no
-- resolution order — no "applies everywhere" environment, no fall-back from an environment to a
-- shared set. A read for an environment sees that environment's rows and nothing else, which is
-- the whole of the behaviour and is readable off the primary key. The consequence is worth stating
-- because a user meets it immediately: a NEWLY REGISTERED ENVIRONMENT STARTS WITH NO CONFIG, and
-- an app deployed into one comes up with whatever its image defaults to until config is set there.
--
-- The NOT NULL DEFAULT 'prod' backfills every existing row to the default environment (ADR-0067
-- §2). Install creates exactly one environment and calls it `prod`, so on an install that never
-- added a second one that is the environment every existing value was already being rendered into.
-- An install that did add one finds its config under `prod` and sets what the other environment
-- needs there; the values are non-secret and readable with `burrow app config`, so nothing is lost
-- and nothing has to be guessed.
ALTER TABLE app_env ADD COLUMN environment TEXT NOT NULL DEFAULT 'prod';

-- The primary key moves with the column. It is the key, not a uniqueness nicety: it is what makes
-- one app's `LOG_LEVEL` in staging a different row from its `LOG_LEVEL` in production, and what
-- makes the upsert a write's ON CONFLICT target land on the right one.
ALTER TABLE app_env DROP CONSTRAINT app_env_pkey;
ALTER TABLE app_env ADD PRIMARY KEY (app, environment, key);

-- +goose Down
-- Going back collapses a per-environment store into an app-global one, and only the default
-- environment's rows can survive that: two environments' values for one key are two rows that the
-- old primary key cannot hold, and there is no basis in the schema for picking between them. The
-- default environment is the one an install that never added another has, so keeping it is the
-- choice that leaves an ordinary install exactly as it was.
DELETE FROM app_env WHERE environment <> 'prod';

ALTER TABLE app_env DROP CONSTRAINT app_env_pkey;
ALTER TABLE app_env DROP COLUMN environment;
ALTER TABLE app_env ADD PRIMARY KEY (app, key);

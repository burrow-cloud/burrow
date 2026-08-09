-- +goose Up
-- rollout records what the deploy OBSERVED of a release's rollout, beside the status that records
-- what Burrow did with it (ADR-0092 §4).
--
-- The two are different questions and `status` cannot answer both. `status` is the registry's own
-- record: which release was applied, and which one `burrow app rollback` returns to — it walks back
-- from the newest `deployed` row, so that word has to keep naming the release Burrow last applied.
-- Whether the pods of that release ever became ready is the cluster's answer, and until this column
-- existed the record had nowhere to put it: a rollout whose new pod never passed its readiness probe
-- was stored, and shown in `burrow app history`, as plain `deployed` (issue #546).
--
-- The values are the closed set controlplane.ReleaseRollout declares: '' (the deploy did not wait,
-- so the outcome is unknown), 'settled' (the new replicas became ready), 'unsettled' (they had not
-- when the deploy stopped waiting). The DEFAULT backfills every existing row to '', which is exact
-- rather than a guess: no deploy before this waited for its own report, so no earlier row has an
-- observation to record.
ALTER TABLE releases ADD COLUMN rollout TEXT NOT NULL DEFAULT '';

-- rollout_reason names why an unsettled rollout did not settle, from the closed vocabulary
-- ADR-0074 §2 enumerates (ImagePullBackOff, CrashLoopBackOff, Unschedulable, and the rest), so the
-- history says what went wrong and not merely that something did. Empty on every other row.
ALTER TABLE releases ADD COLUMN rollout_reason TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE releases DROP COLUMN rollout_reason;
ALTER TABLE releases DROP COLUMN rollout;

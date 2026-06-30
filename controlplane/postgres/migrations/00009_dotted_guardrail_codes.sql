-- +goose Up
-- Rename the guardrail codes from flat underscore names to the dotted resource.operation
-- convention (e.g. app_delete -> app.delete), so they are hierarchical and forward-compatible
-- with a future per-environment prefix (e.g. prod.app.delete). guardrail_policy holds only the
-- operator's set dispositions, so carry each one over by its primary key. audit_log is left
-- untouched: its guardrail_code values are a historical record of what was true at write time.
UPDATE guardrail_policy SET code = 'app.replica_ceiling' WHERE code = 'replica_ceiling';
UPDATE guardrail_policy SET code = 'app.scale_to_zero'   WHERE code = 'scale_to_zero';
UPDATE guardrail_policy SET code = 'app.expose_public'   WHERE code = 'expose_public';
UPDATE guardrail_policy SET code = 'dns.write'           WHERE code = 'dns_write';
UPDATE guardrail_policy SET code = 'dns.delete'          WHERE code = 'dns_delete';
UPDATE guardrail_policy SET code = 'addon.install'       WHERE code = 'addon_install';
UPDATE guardrail_policy SET code = 'addon.remove'        WHERE code = 'addon_remove';
UPDATE guardrail_policy SET code = 'addon.detach'        WHERE code = 'addon_detach';
UPDATE guardrail_policy SET code = 'addon.restore'       WHERE code = 'addon_restore';
UPDATE guardrail_policy SET code = 'app.delete'          WHERE code = 'app_delete';
UPDATE guardrail_policy SET code = 'app.rollback'        WHERE code = 'rollback';

-- +goose Down
UPDATE guardrail_policy SET code = 'replica_ceiling' WHERE code = 'app.replica_ceiling';
UPDATE guardrail_policy SET code = 'scale_to_zero'   WHERE code = 'app.scale_to_zero';
UPDATE guardrail_policy SET code = 'expose_public'   WHERE code = 'app.expose_public';
UPDATE guardrail_policy SET code = 'dns_write'       WHERE code = 'dns.write';
UPDATE guardrail_policy SET code = 'dns_delete'      WHERE code = 'dns.delete';
UPDATE guardrail_policy SET code = 'addon_install'   WHERE code = 'addon.install';
UPDATE guardrail_policy SET code = 'addon_remove'    WHERE code = 'addon.remove';
UPDATE guardrail_policy SET code = 'addon_detach'    WHERE code = 'addon.detach';
UPDATE guardrail_policy SET code = 'addon_restore'   WHERE code = 'addon.restore';
UPDATE guardrail_policy SET code = 'app_delete'      WHERE code = 'app.delete';
UPDATE guardrail_policy SET code = 'rollback'        WHERE code = 'app.rollback';

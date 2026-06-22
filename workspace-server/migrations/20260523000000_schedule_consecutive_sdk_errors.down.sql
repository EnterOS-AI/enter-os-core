-- migration: 20260523000000_schedule_consecutive_sdk_errors.down.sql
-- Reverts #1696 fix for #1696 (consecutive_sdk_errors column)

ALTER TABLE workspace_schedules DROP COLUMN IF EXISTS consecutive_sdk_errors;
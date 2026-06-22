-- Issue #958: Strip CRLF from cron prompts inserted from Windows org-template files.
-- Carriage returns cause empty agent responses and phantom-producing detection.
UPDATE workspace_schedules SET prompt = REPLACE(prompt, E'\r', '') WHERE prompt LIKE E'%\r%';

-- core#2724: revert the ack-first seed. Operator-edited rows are
-- preserved (we only delete the row with the exact title we
-- created in the up-migration).
DELETE FROM platform_instructions
WHERE scope = 'global'
  AND scope_target IS NULL
  AND title = 'Acknowledge-first responsiveness';

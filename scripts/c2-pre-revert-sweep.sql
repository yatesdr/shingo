-- c2-pre-revert-sweep.sql — run against the EDGE sqlite DB BEFORE deploying a
-- build that predates C(ii) (the supply-widening / awaiting_material change).
--
-- Why: C(ii) added two changeover_node_task states — awaiting_material
-- (non-terminal) and abandoned (terminal). Older builds don't know either:
-- an awaiting_material row would render button-less and block the completion
-- gate with no exit, and an abandoned row would read as NON-terminal there
-- (IsTerminal predates it), un-completing a changeover the operator already
-- settled. Sweeping both to 'error' puts them in a state every build renders
-- with a Retry/Skip affordance, which is the honest downgrade: operator
-- attention required.
--
-- Idempotent; safe to run when no rows match. Not needed when rolling
-- FORWARD onto C(ii) — old states load fine under the new predicate.
UPDATE changeover_node_tasks
   SET state = 'error'
 WHERE state IN ('awaiting_material', 'abandoned');

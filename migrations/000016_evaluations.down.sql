DROP INDEX IF EXISTS idx_lss_last_evaluation;
ALTER TABLE proficiency_estimates DROP CONSTRAINT IF EXISTS fk_pe_source_evaluation;
ALTER TABLE learner_skill_states DROP CONSTRAINT IF EXISTS fk_lss_last_evaluation;
DROP TABLE IF EXISTS evaluation_findings;
DROP TABLE IF EXISTS evaluations;

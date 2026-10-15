-- seed/0010_topics.sql: 5 topics (spec 6.13.4). Idempotent: literal ids, ON CONFLICT DO NOTHING.
BEGIN;

INSERT INTO topics (id, code, name, name_ja, description, icon_key, sort_order, status) VALUES
  ('01920000-0001-7000-8000-000000000001', 'JOB_INTERVIEW', 'Job Interview', '就職面接', 'Practice interviews at Japanese companies.', 'briefcase', 10, 'ACTIVE'),
  ('01920000-0001-7000-8000-000000000002', 'DAILY_ACTIVITIES', 'Daily Activities', '日常生活', 'Talk about everyday routines, plans and work.', 'sun', 20, 'ACTIVE'),
  ('01920000-0001-7000-8000-000000000003', 'HOBBIES', 'Hobbies', '趣味', 'Share and discuss hobbies and free-time activities.', 'palette', 30, 'ACTIVE'),
  ('01920000-0001-7000-8000-000000000004', 'JAPANESE_CULTURE', 'Japanese Culture', '日本の文化', 'Food, festivals and customs of Japan.', 'torii', 40, 'ACTIVE'),
  ('01920000-0001-7000-8000-000000000005', 'FREE_TOPIC', 'Free Topic', 'フリートーク', 'Open conversation about anything you like.', 'chat', 50, 'ACTIVE')
ON CONFLICT (id) DO NOTHING;

COMMIT;

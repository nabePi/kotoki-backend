-- seed/0060_scenarios.sql: 13 scenarios with version 1 PUBLISHED (spec 6.13.4). Idempotent.
BEGIN;

-- 1. FRESH_GRADUATE_INTERVIEW
INSERT INTO scenarios (id, topic_id, code, name, name_ja, description, sort_order, status)
VALUES ('01920000-0002-7000-8000-000000000001', '01920000-0001-7000-8000-000000000001', 'FRESH_GRADUATE_INTERVIEW', 'Fresh Graduate Interview', '新卒採用の面接', 'A first-round interview for a new graduate position at a Japanese company.', 10, 'ACTIVE')
ON CONFLICT (id) DO NOTHING;

INSERT INTO scenario_versions (
    id, scenario_id, version, status, difficulty_level, recommended_level_min, recommended_level_max,
    situation, ai_role, user_role, goal, tasks, constraints, grammar_targets, vocab_hints,
    evaluation_focus, formality, opening_line, visual_prompt, notes, published_at)
VALUES ('01920000-0003-7000-8000-000000000001', '01920000-0002-7000-8000-000000000001', 1, 'PUBLISHED', 2, 1.0, 2.5,
    'A first-round interview for a new graduate position at a Japanese company. The interview is held in a small meeting room at the company''s head office. The interviewer wants to know who the candidate is, what they studied and why they want to join.',
    'HR interviewer at a Japanese company interviewing a new graduate',
    'New graduate applying for an entry-level position',
    'The learner introduces themselves, talks about their studies and explains why they want to work at the company.',
    '["Introduce yourself briefly", "Talk about what you studied", "Explain why you want to join this company", "Ask the interviewer one question"]'::jsonb,
    '["Conduct the whole interview in Japanese", "Ask one question at a time", "Behave like a realistic, polite Japanese interviewer", "If the learner gives a very short answer, ask a natural follow-up"]'::jsonb,
    '["〜と申します", "〜を勉強しておりました", "〜たいと思っております"]'::jsonb,
    '[{"term_ja": "自己紹介", "reading": "じこしょうかい", "meaning": "self-introduction"}, {"term_ja": "志望動機", "reading": "しぼうどうき", "meaning": "reason for applying"}, {"term_ja": "専攻", "reading": "せんこう", "meaning": "major (field of study)"}, {"term_ja": "大学", "reading": "だいがく", "meaning": "university"}, {"term_ja": "頑張る", "reading": "がんばる", "meaning": "to do one''s best"}]'::jsonb,
    ARRAY['VOCABULARY', 'FLUENCY'], 'KEIGO',
    '本日はお越しいただき、ありがとうございます。まず、自己紹介をお願いします。',
    '{"headline": "You enter the interview room", "body": "The interviewer is waiting for you. Greet them and introduce yourself to begin.", "starter_phrases_ja": ["本日はよろしくお願いいたします。", "はじめまして、〇〇と申します。"], "image_key": "interview_room"}'::jsonb,
    'Initial version', now())
ON CONFLICT (id) DO NOTHING;

-- 2. JAPANESE_COMPANY_INTERVIEW
INSERT INTO scenarios (id, topic_id, code, name, name_ja, description, sort_order, status)
VALUES ('01920000-0002-7000-8000-000000000002', '01920000-0001-7000-8000-000000000001', 'JAPANESE_COMPANY_INTERVIEW', 'Japanese Company Interview', '日系企業の面接', 'An interview for a mid-career position at a Japanese company.', 20, 'ACTIVE')
ON CONFLICT (id) DO NOTHING;

INSERT INTO scenario_versions (
    id, scenario_id, version, status, difficulty_level, recommended_level_min, recommended_level_max,
    situation, ai_role, user_role, goal, tasks, constraints, grammar_targets, vocab_hints,
    evaluation_focus, formality, opening_line, visual_prompt, notes, published_at)
VALUES ('01920000-0003-7000-8000-000000000002', '01920000-0002-7000-8000-000000000002', 1, 'PUBLISHED', 3, 2.0, 3.5,
    'An interview for a mid-career position at a Japanese company. The interviewer is a department manager who wants to understand the candidate''s work experience, strengths and how they would work in a Japanese team.',
    'Department manager interviewing a mid-career candidate',
    'Candidate with some work experience applying for a new role',
    'The learner explains their work experience, describes their strengths and talks about working in a team.',
    '["Summarise your work experience", "Describe one strength and give an example", "Explain how you work with a team", "Talk about why you want to work in Japan", "Ask the interviewer one question about the job"]'::jsonb,
    '["Conduct the whole interview in Japanese", "Ask one question at a time", "Stay polite, calm and attentive like a real Japanese manager", "Follow up on what the learner says before changing topic"]'::jsonb,
    '["〜てまいりました", "〜ことができます", "〜と考えております"]'::jsonb,
    '[{"term_ja": "経験", "reading": "けいけん", "meaning": "experience"}, {"term_ja": "強み", "reading": "つよみ", "meaning": "strength"}, {"term_ja": "チーム", "reading": "ちーむ", "meaning": "team"}, {"term_ja": "貢献", "reading": "こうけん", "meaning": "contribution"}, {"term_ja": "責任", "reading": "せきにん", "meaning": "responsibility"}]'::jsonb,
    ARRAY['VOCABULARY', 'FLUENCY', 'INTERACTION_COMPLEXITY'], 'KEIGO',
    '本日はお忙しい中、お越しいただきありがとうございます。これまでのご経験について教えていただけますか。',
    '{"headline": "A meeting with the manager", "body": "The department manager greets you. Start by thanking them and summarising your experience.", "starter_phrases_ja": ["本日はお時間をいただき、ありがとうございます。", "これまでの経験についてお話しします。"], "image_key": "manager_office"}'::jsonb,
    'Initial version', now())
ON CONFLICT (id) DO NOTHING;

-- 3. BACKEND_ENGINEER_INTERVIEW
INSERT INTO scenarios (id, topic_id, code, name, name_ja, description, sort_order, status)
VALUES ('01920000-0002-7000-8000-000000000003', '01920000-0001-7000-8000-000000000001', 'BACKEND_ENGINEER_INTERVIEW', 'Backend Engineer Interview', 'バックエンドエンジニアの面接', 'A technical interview for a backend engineer role at a Japanese tech company.', 30, 'ACTIVE')
ON CONFLICT (id) DO NOTHING;

INSERT INTO scenario_versions (
    id, scenario_id, version, status, difficulty_level, recommended_level_min, recommended_level_max,
    situation, ai_role, user_role, goal, tasks, constraints, grammar_targets, vocab_hints,
    evaluation_focus, formality, opening_line, visual_prompt, notes, published_at)
VALUES ('01920000-0003-7000-8000-000000000003', '01920000-0002-7000-8000-000000000003', 1, 'PUBLISHED', 4, 3.0, 4.5,
    'A second-round technical interview for a backend engineer position at a mid-sized SaaS company in Tokyo. The interview is held in a meeting room and lasts about as long as this session. The company uses Go and PostgreSQL and values clear communication.',
    'Engineering manager at the company who is interviewing the candidate',
    'Candidate applying for the backend engineer position',
    'The learner introduces themselves, explains their backend experience, and explains one technical decision and its trade-offs in Japanese.',
    '["Introduce yourself briefly", "Describe a backend system you have built", "Explain one technical decision and why you made it", "Answer at least one follow-up question about trade-offs", "Ask the interviewer one question about the team"]'::jsonb,
    '["Conduct the whole interview in Japanese", "Ask one question at a time", "Behave like a realistic Japanese interviewer: polite, calm, and attentive", "If the learner gives a very short answer, ask a natural follow-up instead of moving on"]'::jsonb,
    '["〜たことがあります", "〜ために", "〜と考えております"]'::jsonb,
    '[{"term_ja": "経験", "reading": "けいけん", "meaning": "experience"}, {"term_ja": "設計", "reading": "せっけい", "meaning": "design (of a system)"}, {"term_ja": "担当", "reading": "たんとう", "meaning": "being in charge of"}, {"term_ja": "課題", "reading": "かだい", "meaning": "challenge, issue"}, {"term_ja": "改善", "reading": "かいぜん", "meaning": "improvement"}]'::jsonb,
    ARRAY['VOCABULARY', 'FLUENCY', 'INTERACTION_COMPLEXITY'], 'KEIGO',
    '本日はお越しいただきありがとうございます。まずは簡単に自己紹介をお願いできますか。',
    '{"headline": "You enter the interview room", "body": "The engineering manager is waiting for you. Greet them and introduce yourself to start the interview.", "starter_phrases_ja": ["本日はよろしくお願いいたします。", "はじめまして、〇〇と申します。"], "image_key": "interview_room"}'::jsonb,
    'Initial version', now())
ON CONFLICT (id) DO NOTHING;

-- 4. MORNING_ROUTINE
INSERT INTO scenarios (id, topic_id, code, name, name_ja, description, sort_order, status)
VALUES ('01920000-0002-7000-8000-000000000004', '01920000-0001-7000-8000-000000000002', 'MORNING_ROUTINE', 'Morning Routine', '朝のルーティン', 'Chat about what you do from waking up until you leave home.', 10, 'ACTIVE')
ON CONFLICT (id) DO NOTHING;

INSERT INTO scenario_versions (
    id, scenario_id, version, status, difficulty_level, recommended_level_min, recommended_level_max,
    situation, ai_role, user_role, goal, tasks, constraints, grammar_targets, vocab_hints,
    evaluation_focus, formality, opening_line, visual_prompt, notes, published_at)
VALUES ('01920000-0003-7000-8000-000000000004', '01920000-0002-7000-8000-000000000004', 1, 'PUBLISHED', 1, 0.5, 1.5,
    'A friendly chat about daily mornings. The two of you are talking over coffee before the day starts, comparing what time you get up and what you usually do before leaving home.',
    'A friendly acquaintance asking about morning habits',
    'A person describing their own morning',
    'The learner describes their morning routine in simple Japanese and asks a question back.',
    '["Say what time you wake up", "Describe what you eat or drink in the morning", "Say how you go to work or school", "Ask the other person about their morning"]'::jsonb,
    '["Speak only in simple, short Japanese sentences", "Ask one question at a time", "Use polite です/ます form", "If the learner is stuck, offer an easy choice such as AかBか"]'::jsonb,
    '["〜に起きます", "〜を食べます", "〜てから、〜ます"]'::jsonb,
    '[{"term_ja": "朝", "reading": "あさ", "meaning": "morning"}, {"term_ja": "起きる", "reading": "おきる", "meaning": "to wake up"}, {"term_ja": "朝ごはん", "reading": "あさごはん", "meaning": "breakfast"}, {"term_ja": "歯みがき", "reading": "はみがき", "meaning": "tooth brushing"}, {"term_ja": "出かける", "reading": "でかける", "meaning": "to go out"}]'::jsonb,
    ARRAY['GRAMMAR', 'FLUENCY'], 'POLITE',
    'おはようございます。いつも何時に起きますか。',
    '{"headline": "Good morning!", "body": "Your conversation partner asks about your morning. Answer in simple Japanese.", "starter_phrases_ja": ["六時に起きます。", "朝ごはんにパンを食べます。"], "image_key": "morning_kitchen"}'::jsonb,
    'Initial version', now())
ON CONFLICT (id) DO NOTHING;

-- 5. WEEKEND_PLANS
INSERT INTO scenarios (id, topic_id, code, name, name_ja, description, sort_order, status)
VALUES ('01920000-0002-7000-8000-000000000005', '01920000-0001-7000-8000-000000000002', 'WEEKEND_PLANS', 'Weekend Plans', '週末の予定', 'Talk about what you did or will do on the weekend.', 20, 'ACTIVE')
ON CONFLICT (id) DO NOTHING;

INSERT INTO scenario_versions (
    id, scenario_id, version, status, difficulty_level, recommended_level_min, recommended_level_max,
    situation, ai_role, user_role, goal, tasks, constraints, grammar_targets, vocab_hints,
    evaluation_focus, formality, opening_line, visual_prompt, notes, published_at)
VALUES ('01920000-0003-7000-8000-000000000005', '01920000-0002-7000-8000-000000000005', 1, 'PUBLISHED', 1, 0.5, 1.5,
    'It is Friday afternoon and you are chatting about the weekend. You talk about what you did last weekend and what you plan to do this weekend.',
    'A friendly acquaintance asking about weekend plans',
    'A person talking about their weekend',
    'The learner says what they plan to do this weekend and what they did last weekend.',
    '["Say what you will do this weekend", "Say where you will go", "Say what you did last weekend", "Ask the other person about their plans"]'::jsonb,
    '["Speak only in simple, short Japanese sentences", "Ask one question at a time", "Use polite です/ます form", "Mix questions about the future and the past"]'::jsonb,
    '["〜に行きます", "〜ました", "〜つもりです"]'::jsonb,
    '[{"term_ja": "週末", "reading": "しゅうまつ", "meaning": "weekend"}, {"term_ja": "予定", "reading": "よてい", "meaning": "plan"}, {"term_ja": "買い物", "reading": "かいもの", "meaning": "shopping"}, {"term_ja": "映画", "reading": "えいが", "meaning": "movie"}, {"term_ja": "友達", "reading": "ともだち", "meaning": "friend"}]'::jsonb,
    ARRAY['GRAMMAR', 'FLUENCY'], 'POLITE',
    'こんにちは。今週の週末は何をしますか。',
    '{"headline": "Friday afternoon", "body": "Your conversation partner asks about your weekend. Tell them your plans.", "starter_phrases_ja": ["映画を見に行きます。", "友達と買い物をします。"], "image_key": "weekend_street"}'::jsonb,
    'Initial version', now())
ON CONFLICT (id) DO NOTHING;

-- 6. TALKING_ABOUT_WORK
INSERT INTO scenarios (id, topic_id, code, name, name_ja, description, sort_order, status)
VALUES ('01920000-0002-7000-8000-000000000006', '01920000-0001-7000-8000-000000000002', 'TALKING_ABOUT_WORK', 'Talking About Work', '仕事の話', 'Talk about your job, daily tasks and workplace.', 30, 'ACTIVE')
ON CONFLICT (id) DO NOTHING;

INSERT INTO scenario_versions (
    id, scenario_id, version, status, difficulty_level, recommended_level_min, recommended_level_max,
    situation, ai_role, user_role, goal, tasks, constraints, grammar_targets, vocab_hints,
    evaluation_focus, formality, opening_line, visual_prompt, notes, published_at)
VALUES ('01920000-0003-7000-8000-000000000006', '01920000-0002-7000-8000-000000000006', 1, 'PUBLISHED', 2, 1.0, 2.5,
    'You meet a new acquaintance at an after-work gathering and chat about your jobs: what you do, what a typical day looks like and what you like or find hard about it.',
    'A friendly acquaintance curious about the learner''s job',
    'A professional talking about their own work',
    'The learner explains what their job is, what they do in a typical day and what they like about it.',
    '["Say what your job is", "Describe what you do in a typical day", "Say what you like about your work", "Say one thing that is difficult", "Ask the other person about their job"]'::jsonb,
    '["Use polite です/ます form", "Ask one question at a time", "React naturally to what the learner says before asking the next question", "Avoid difficult technical vocabulary unless the learner uses it first"]'::jsonb,
    '["〜をしています", "〜のが好きです", "〜ので、〜"]'::jsonb,
    '[{"term_ja": "仕事", "reading": "しごと", "meaning": "work, job"}, {"term_ja": "会社", "reading": "かいしゃ", "meaning": "company"}, {"term_ja": "同僚", "reading": "どうりょう", "meaning": "colleague"}, {"term_ja": "残業", "reading": "ざんぎょう", "meaning": "overtime"}, {"term_ja": "忙しい", "reading": "いそがしい", "meaning": "busy"}]'::jsonb,
    ARRAY['VOCABULARY', 'FLUENCY'], 'POLITE',
    'はじめまして。お仕事は何をしていますか。',
    '{"headline": "An after-work gathering", "body": "You meet someone new. Tell them about your job.", "starter_phrases_ja": ["エンジニアです。", "会社で働いています。"], "image_key": "izakaya"}'::jsonb,
    'Initial version', now())
ON CONFLICT (id) DO NOTHING;

-- 7. FAVORITE_HOBBY
INSERT INTO scenarios (id, topic_id, code, name, name_ja, description, sort_order, status)
VALUES ('01920000-0002-7000-8000-000000000007', '01920000-0001-7000-8000-000000000003', 'FAVORITE_HOBBY', 'Favorite Hobby', '好きな趣味', 'Talk about your favorite hobby and why you enjoy it.', 10, 'ACTIVE')
ON CONFLICT (id) DO NOTHING;

INSERT INTO scenario_versions (
    id, scenario_id, version, status, difficulty_level, recommended_level_min, recommended_level_max,
    situation, ai_role, user_role, goal, tasks, constraints, grammar_targets, vocab_hints,
    evaluation_focus, formality, opening_line, visual_prompt, notes, published_at)
VALUES ('01920000-0003-7000-8000-000000000007', '01920000-0002-7000-8000-000000000007', 1, 'PUBLISHED', 1, 0.5, 1.5,
    'A relaxed chat about free time. The two of you talk about what you like to do on days off and why you enjoy it.',
    'A friendly acquaintance curious about the learner''s hobby',
    'A person talking about their hobby',
    'The learner says what their hobby is, how often they do it and why they like it.',
    '["Say what your hobby is", "Say how often you do it", "Say why you like it", "Ask the other person about their hobby"]'::jsonb,
    '["Speak only in simple, short Japanese sentences", "Ask one question at a time", "Use polite です/ます form", "Show interest and react to what the learner says"]'::jsonb,
    '["〜が好きです", "〜ことが好きです", "〜に〜回"]'::jsonb,
    '[{"term_ja": "趣味", "reading": "しゅみ", "meaning": "hobby"}, {"term_ja": "音楽", "reading": "おんがく", "meaning": "music"}, {"term_ja": "料理", "reading": "りょうり", "meaning": "cooking"}, {"term_ja": "よく", "reading": "よく", "meaning": "often"}, {"term_ja": "楽しい", "reading": "たのしい", "meaning": "fun"}]'::jsonb,
    ARRAY['GRAMMAR', 'FLUENCY'], 'POLITE',
    'こんにちは。趣味は何ですか。',
    '{"headline": "A chat about hobbies", "body": "Your conversation partner asks about your hobby. Tell them what you enjoy.", "starter_phrases_ja": ["趣味は音楽です。", "週末によく料理をします。"], "image_key": "hobby_desk"}'::jsonb,
    'Initial version', now())
ON CONFLICT (id) DO NOTHING;

-- 8. STARTING_NEW_HOBBY
INSERT INTO scenarios (id, topic_id, code, name, name_ja, description, sort_order, status)
VALUES ('01920000-0002-7000-8000-000000000008', '01920000-0001-7000-8000-000000000003', 'STARTING_NEW_HOBBY', 'Starting a New Hobby', '新しい趣味を始める', 'Talk about a hobby you want to start and how to begin.', 20, 'ACTIVE')
ON CONFLICT (id) DO NOTHING;

INSERT INTO scenario_versions (
    id, scenario_id, version, status, difficulty_level, recommended_level_min, recommended_level_max,
    situation, ai_role, user_role, goal, tasks, constraints, grammar_targets, vocab_hints,
    evaluation_focus, formality, opening_line, visual_prompt, notes, published_at)
VALUES ('01920000-0003-7000-8000-000000000008', '01920000-0002-7000-8000-000000000008', 1, 'PUBLISHED', 2, 1.0, 2.5,
    'You tell a friend that you would like to start a new hobby. Your friend asks what it is, why you are interested and how you plan to begin.',
    'A friendly person asking questions and giving encouragement',
    'A person wanting to start a new hobby',
    'The learner explains which hobby they want to start, why, and how they will begin.',
    '["Say which hobby you want to start", "Explain why you are interested", "Say what you need to get started", "Talk about when you will begin"]'::jsonb,
    '["Use polite です/ます form", "Ask one question at a time", "Give short encouraging reactions", "If the learner is stuck, suggest an easy example"]'::jsonb,
    '["〜てみたいです", "〜ために〜", "〜たら、〜"]'::jsonb,
    '[{"term_ja": "始める", "reading": "はじめる", "meaning": "to start"}, {"term_ja": "道具", "reading": "どうぐ", "meaning": "tool, equipment"}, {"term_ja": "教室", "reading": "きょうしつ", "meaning": "class"}, {"term_ja": "初心者", "reading": "しょしんしゃ", "meaning": "beginner"}, {"term_ja": "続ける", "reading": "つづける", "meaning": "to continue"}]'::jsonb,
    ARRAY['VOCABULARY', 'GRAMMAR'], 'POLITE',
    '最近、何か新しく始めたいことはありますか。',
    '{"headline": "Thinking of a new hobby", "body": "Your friend asks if you want to start something new. Tell them your idea.", "starter_phrases_ja": ["ギターを始めたいです。", "初心者ですが、やってみたいです。"], "image_key": "hobby_shop"}'::jsonb,
    'Initial version', now())
ON CONFLICT (id) DO NOTHING;

-- 9. RECOMMENDING_HOBBY
INSERT INTO scenarios (id, topic_id, code, name, name_ja, description, sort_order, status)
VALUES ('01920000-0002-7000-8000-000000000009', '01920000-0001-7000-8000-000000000003', 'RECOMMENDING_HOBBY', 'Recommending a Hobby', '趣味をすすめる', 'Recommend a hobby to someone and explain why it is worth trying.', 30, 'ACTIVE')
ON CONFLICT (id) DO NOTHING;

INSERT INTO scenario_versions (
    id, scenario_id, version, status, difficulty_level, recommended_level_min, recommended_level_max,
    situation, ai_role, user_role, goal, tasks, constraints, grammar_targets, vocab_hints,
    evaluation_focus, formality, opening_line, visual_prompt, notes, published_at)
VALUES ('01920000-0003-7000-8000-000000000009', '01920000-0002-7000-8000-000000000009', 1, 'PUBLISHED', 3, 2.0, 3.5,
    'A friend is looking for a hobby and asks for your recommendation. You suggest one, explain its good points and answer your friend''s questions and doubts.',
    'A friend looking for a hobby who asks questions and has some doubts',
    'A person recommending a hobby they enjoy',
    'The learner recommends a hobby, explains its good points and answers questions about cost, time and difficulty.',
    '["Recommend one hobby", "Explain two good points", "Answer a question about cost or time", "Respond to a doubt such as it sounds difficult", "Invite your friend to try it together"]'::jsonb,
    '["Use polite です/ます form", "Ask one question at a time", "Raise a natural doubt so the learner has to persuade", "Accept the recommendation gladly at the end"]'::jsonb,
    '["〜といいですよ", "〜し、〜し", "〜てみませんか"]'::jsonb,
    '[{"term_ja": "おすすめ", "reading": "おすすめ", "meaning": "recommendation"}, {"term_ja": "費用", "reading": "ひよう", "meaning": "cost"}, {"term_ja": "時間", "reading": "じかん", "meaning": "time"}, {"term_ja": "簡単", "reading": "かんたん", "meaning": "easy"}, {"term_ja": "一緒に", "reading": "いっしょに", "meaning": "together"}]'::jsonb,
    ARRAY['VOCABULARY', 'INTERACTION_COMPLEXITY'], 'POLITE',
    '最近、新しい趣味を探しているんです。何かおすすめはありますか。',
    '{"headline": "A friend needs ideas", "body": "Your friend is looking for a hobby. Recommend one that you enjoy.", "starter_phrases_ja": ["ランニングがおすすめですよ。", "一緒にやってみませんか。"], "image_key": "hobby_cafe"}'::jsonb,
    'Initial version', now())
ON CONFLICT (id) DO NOTHING;

-- 10. FOOD_CULTURE
INSERT INTO scenarios (id, topic_id, code, name, name_ja, description, sort_order, status)
VALUES ('01920000-0002-7000-8000-000000000010', '01920000-0001-7000-8000-000000000004', 'FOOD_CULTURE', 'Japanese Food Culture', '日本の食文化', 'Talk about Japanese food, meals and eating customs.', 10, 'ACTIVE')
ON CONFLICT (id) DO NOTHING;

INSERT INTO scenario_versions (
    id, scenario_id, version, status, difficulty_level, recommended_level_min, recommended_level_max,
    situation, ai_role, user_role, goal, tasks, constraints, grammar_targets, vocab_hints,
    evaluation_focus, formality, opening_line, visual_prompt, notes, published_at)
VALUES ('01920000-0003-7000-8000-000000000010', '01920000-0002-7000-8000-000000000010', 1, 'PUBLISHED', 2, 1.0, 2.5,
    'You are sitting in a restaurant and chatting about Japanese food: what you like, what you have tried and how people eat in Japan.',
    'A Japanese friend explaining and asking about food',
    'A visitor curious about Japanese food',
    'The learner talks about the Japanese food they like or want to try and asks about eating customs.',
    '["Say which Japanese food you like", "Say what you want to try", "Ask about a custom such as saying いただきます", "Compare with the food of your own country"]'::jsonb,
    '["Use polite です/ます form", "Ask one question at a time", "Explain a custom briefly in simple Japanese", "React with interest to the learner''s opinions"]'::jsonb,
    '["〜たことがあります", "〜と〜のどちらが〜", "〜に似ています"]'::jsonb,
    '[{"term_ja": "寿司", "reading": "すし", "meaning": "sushi"}, {"term_ja": "味噌汁", "reading": "みそしる", "meaning": "miso soup"}, {"term_ja": "箸", "reading": "はし", "meaning": "chopsticks"}, {"term_ja": "いただきます", "reading": "いただきます", "meaning": "phrase said before eating"}, {"term_ja": "おいしい", "reading": "おいしい", "meaning": "delicious"}]'::jsonb,
    ARRAY['VOCABULARY', 'NATURALNESS'], 'POLITE',
    '日本の食べ物では、何が好きですか。',
    '{"headline": "At a restaurant", "body": "Your friend asks about Japanese food. Tell them what you like.", "starter_phrases_ja": ["寿司が好きです。", "ラーメンを食べてみたいです。"], "image_key": "restaurant_table"}'::jsonb,
    'Initial version', now())
ON CONFLICT (id) DO NOTHING;

-- 11. FESTIVALS
INSERT INTO scenarios (id, topic_id, code, name, name_ja, description, sort_order, status)
VALUES ('01920000-0002-7000-8000-000000000011', '01920000-0001-7000-8000-000000000004', 'FESTIVALS', 'Japanese Festivals', '日本のお祭り', 'Talk about Japanese festivals and seasonal events.', 20, 'ACTIVE')
ON CONFLICT (id) DO NOTHING;

INSERT INTO scenario_versions (
    id, scenario_id, version, status, difficulty_level, recommended_level_min, recommended_level_max,
    situation, ai_role, user_role, goal, tasks, constraints, grammar_targets, vocab_hints,
    evaluation_focus, formality, opening_line, visual_prompt, notes, published_at)
VALUES ('01920000-0003-7000-8000-000000000011', '01920000-0002-7000-8000-000000000011', 1, 'PUBLISHED', 3, 2.0, 3.5,
    'You are talking with a friend about Japanese festivals and seasonal events such as summer festivals and hanabi, and about festivals in your own country.',
    'A Japanese friend who enjoys festivals',
    'A person curious about festivals',
    'The learner talks about a festival they know or want to see, describes what people do there and compares it with a festival from their country.',
    '["Name a festival you know or want to see", "Describe what people do there", "Say what you would enjoy most", "Compare it with a festival in your country", "Ask about the friend''s favorite festival"]'::jsonb,
    '["Use polite です/ます form", "Ask one question at a time", "Share a short personal story to keep the chat natural", "Explain one cultural point simply when relevant"]'::jsonb,
    '["〜そうです", "〜たり〜たりします", "〜ほど〜ない"]'::jsonb,
    '[{"term_ja": "お祭り", "reading": "おまつり", "meaning": "festival"}, {"term_ja": "花火", "reading": "はなび", "meaning": "fireworks"}, {"term_ja": "浴衣", "reading": "ゆかた", "meaning": "summer kimono"}, {"term_ja": "屋台", "reading": "やたい", "meaning": "food stall"}, {"term_ja": "季節", "reading": "きせつ", "meaning": "season"}]'::jsonb,
    ARRAY['VOCABULARY', 'INTERACTION_COMPLEXITY'], 'POLITE',
    '夏になると、日本ではあちこちでお祭りがありますね。お祭りに行ったことはありますか。',
    '{"headline": "Summer festival season", "body": "Your friend talks about festivals. Share what you know or what you want to experience.", "starter_phrases_ja": ["花火を見てみたいです。", "私の国にもお祭りがあります。"], "image_key": "festival_street"}'::jsonb,
    'Initial version', now())
ON CONFLICT (id) DO NOTHING;

-- 12. DAILY_CUSTOMS
INSERT INTO scenarios (id, topic_id, code, name, name_ja, description, sort_order, status)
VALUES ('01920000-0002-7000-8000-000000000012', '01920000-0001-7000-8000-000000000004', 'DAILY_CUSTOMS', 'Daily Customs in Japan', '日本の生活習慣', 'Discuss everyday customs and manners in Japan.', 30, 'ACTIVE')
ON CONFLICT (id) DO NOTHING;

INSERT INTO scenario_versions (
    id, scenario_id, version, status, difficulty_level, recommended_level_min, recommended_level_max,
    situation, ai_role, user_role, goal, tasks, constraints, grammar_targets, vocab_hints,
    evaluation_focus, formality, opening_line, visual_prompt, notes, published_at)
VALUES ('01920000-0003-7000-8000-000000000012', '01920000-0002-7000-8000-000000000012', 1, 'PUBLISHED', 4, 3.0, 4.5,
    'You and a Japanese friend are discussing everyday customs and manners in Japan, such as removing shoes, bowing, gift giving and train etiquette, and how they differ from your own country.',
    'A thoughtful Japanese friend discussing customs and manners',
    'A person interested in Japanese customs',
    'The learner explains customs they have noticed, gives opinions about them and compares them with customs in their own country.',
    '["Describe a Japanese custom you have noticed", "Give your opinion about it", "Explain a similar or different custom in your country", "Ask why a custom exists", "Summarize what you learned"]'::jsonb,
    '["Use polite です/ます form with natural softening", "Ask one question at a time", "Give your own view and ask for the learner''s reason", "Avoid slang unless the learner uses it first"]'::jsonb,
    '["〜ということです", "〜べきだと思います", "〜わけではありません"]'::jsonb,
    '[{"term_ja": "習慣", "reading": "しゅうかん", "meaning": "custom, habit"}, {"term_ja": "マナー", "reading": "まなー", "meaning": "manners"}, {"term_ja": "お辞儀", "reading": "おじぎ", "meaning": "bow"}, {"term_ja": "遠慮", "reading": "えんりょ", "meaning": "restraint, holding back"}, {"term_ja": "文化", "reading": "ぶんか", "meaning": "culture"}]'::jsonb,
    ARRAY['VOCABULARY', 'NATURALNESS', 'INTERACTION_COMPLEXITY'], 'POLITE',
    '日本に来てから、驚いた習慣はありますか。',
    '{"headline": "Talking about customs", "body": "Your friend asks about customs in Japan. Share what you noticed and why.", "starter_phrases_ja": ["靴を脱ぐ習慣に驚きました。", "私の国とは違います。"], "image_key": "genkan"}'::jsonb,
    'Initial version', now())
ON CONFLICT (id) DO NOTHING;

-- 13. FREE_CONVERSATION
INSERT INTO scenarios (id, topic_id, code, name, name_ja, description, sort_order, status)
VALUES ('01920000-0002-7000-8000-000000000013', '01920000-0001-7000-8000-000000000005', 'FREE_CONVERSATION', 'Free Conversation', 'フリートーク', 'Talk about anything you like.', 10, 'ACTIVE')
ON CONFLICT (id) DO NOTHING;

INSERT INTO scenario_versions (
    id, scenario_id, version, status, difficulty_level, recommended_level_min, recommended_level_max,
    situation, ai_role, user_role, goal, tasks, constraints, grammar_targets, vocab_hints,
    evaluation_focus, formality, opening_line, visual_prompt, notes, published_at)
VALUES ('01920000-0003-7000-8000-000000000013', '01920000-0002-7000-8000-000000000013', 1, 'PUBLISHED', 2, 1.0, 2.5,
    'A casual open conversation with a friendly person. There is no set topic: the conversation follows whatever the learner is interested in, such as daily life, news, travel or food.',
    'A friendly conversation partner who follows the learner''s topics',
    'A person chatting freely',
    'The learner keeps a natural conversation going on topics they choose.',
    '["Pick a topic you want to talk about", "Share your opinion or experience", "Ask the other person at least two questions", "Change the topic once naturally"]'::jsonb,
    '["Use polite です/ます form", "Ask one question at a time", "Follow the learner''s topic and show curiosity", "Share short opinions of your own to keep the chat balanced"]'::jsonb,
    '["〜と思います", "〜んです", "〜ですよね"]'::jsonb,
    '[{"term_ja": "最近", "reading": "さいきん", "meaning": "recently"}, {"term_ja": "話", "reading": "はなし", "meaning": "story, talk"}, {"term_ja": "意見", "reading": "いけん", "meaning": "opinion"}, {"term_ja": "旅行", "reading": "りょこう", "meaning": "trip, travel"}, {"term_ja": "おもしろい", "reading": "おもしろい", "meaning": "interesting"}]'::jsonb,
    ARRAY['FLUENCY', 'NATURALNESS'], 'POLITE',
    'こんにちは。今日は何について話しましょうか。',
    '{"headline": "Open conversation", "body": "Talk about anything you like. Start with a greeting.", "starter_phrases_ja": ["最近、旅行に行きました。", "今日は天気について話したいです。"], "image_key": "free_talk"}'::jsonb,
    'Initial version', now())
ON CONFLICT (id) DO NOTHING;

COMMIT;

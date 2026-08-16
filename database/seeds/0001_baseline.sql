-- SOBH baseline seed: feature flags and the initial news taxonomy (§26, §68).
-- Idempotent — safe to re-run on every deploy.

INSERT INTO feature_flags (key, enabled, rollout_percent, description) VALUES
    ('messaging_enabled',    TRUE,  100, 'Private chats and groups'),
    ('channels_enabled',     TRUE,  100, 'Channels and channel posts'),
    ('communities_enabled',  FALSE, 100, 'Communities grouping channels and groups'),
    ('stories_enabled',      FALSE, 100, 'User and channel stories'),
    ('calls_enabled',        FALSE, 100, 'Voice and video calls'),
    ('group_calls_enabled',  FALSE, 100, 'Multi-party calls'),
    ('secret_chats_enabled', FALSE, 100, 'End-to-end encrypted chats'),
    ('news_enabled',         TRUE,  100, 'News feed and articles'),
    ('breaking_news_enabled',TRUE,  100, 'Breaking news push notifications'),
    ('ai_enabled',           FALSE, 100, 'AI summary, search and transcription'),
    ('ai_translation',       FALSE, 100, 'AI article translation'),
    ('data_export_enabled',  TRUE,  100, 'User-initiated data export')
ON CONFLICT (key) DO NOTHING;

WITH seed(slug, position, fa, en, ar, tr) AS (VALUES
    ('hormozgan',   10, 'هرمزگان',     'Hormozgan',    'هرمزغان',      'Hürmüzgan'),
    ('bandarabbas', 20, 'بندرعباس',    'Bandar Abbas', 'بندر عباس',    'Bender Abbas'),
    ('kish',        30, 'کیش',         'Kish',         'كيش',          'Kiş'),
    ('qeshm',       40, 'قشم',         'Qeshm',        'قشم',          'Kişm'),
    ('minab',       50, 'میناب',       'Minab',        'ميناب',        'Minab'),
    ('iran',        60, 'ایران',       'Iran',         'إيران',        'İran'),
    ('economy',     70, 'اقتصاد',      'Economy',      'اقتصاد',       'Ekonomi'),
    ('sports',      80, 'ورزش',        'Sports',       'رياضة',        'Spor'),
    ('culture',     90, 'فرهنگ',       'Culture',      'ثقافة',        'Kültür'),
    ('society',    100, 'جامعه',       'Society',      'مجتمع',        'Toplum'),
    ('world',      110, 'بین‌الملل',   'World',        'دولي',         'Dünya')
), inserted AS (
    INSERT INTO news_categories (slug, position)
    SELECT slug, position FROM seed
    ON CONFLICT (slug) DO UPDATE SET position = EXCLUDED.position
    RETURNING id, slug
)
INSERT INTO news_category_names (category_id, locale, name)
SELECT i.id, l.locale, l.name
FROM inserted i
JOIN seed s ON s.slug = i.slug
CROSS JOIN LATERAL (VALUES ('fa', s.fa), ('en', s.en), ('ar', s.ar), ('tr', s.tr)) AS l(locale, name)
ON CONFLICT (category_id, locale) DO UPDATE SET name = EXCLUDED.name;

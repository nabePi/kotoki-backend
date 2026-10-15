FROM migrate/migrate:v4.18.3
COPY migrations /migrations
ENTRYPOINT ["migrate", "-path", "/migrations"]
# Job args: ["-database", "$(DB_MIGRATE_URL)", "up"]

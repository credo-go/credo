# Credo reference files

This directory contains versioned, copyable reference files. Unlike the `examples/hello` and `examples/saas` directories, it is not a runnable application.

## Pre-v1 migration status

The [accepted migration preview](../../docs/guides/pre-v1-migration.md) does not change these files' current format. UseI18n remains explicit; the HTTP migration preserves exact message/field keys, plural catalogs and explicit-source validation. The accepted detector is Detect(*Context), memoized on first use. Successful inactive discovery consumes registration; real setup errors do not. Do not add framework-read `middleware.*.enabled` switches to the reference files: optional feature activation belongs in application bootstrap through Use calls. Function-valued configs remain code. See the [example migration map](../README.md) for the runnable applications.

## Configuration

`config/config.yaml` and `config/config.json` describe the same configuration in the two file formats supported by Credo. Copy one format into an application root; using both is only useful when intentionally layering configuration, because Credo merges all discovered config files in JSON, YAML, YML order.

- `server` is read automatically by `credo.New`.
- `i18n` is read when `app.UseI18n()` is called without an explicit file source.
- `databases` and `app` demonstrate application-owned typed snapshots. The `default` and `analytics` entries show that applications may keep multiple named database configurations. Read them at the composition root with `RawConfig.Unmarshal` or `GetConfig[T]`.
- Function-valued settings and programmatic options do not belong in these files.

The TLS paths are empty so the reference remains safe to run as plaintext. Set both paths together to enable file-based TLS.

`config/.env.example` demonstrates environment overrides without containing real credentials. Copy it to `.env` for local development; do not commit secrets.

## Localization

`locales/` contains complete English and Turkish starter catalogs. Copy the directory into an application root and either call `app.UseI18n()` for conventional discovery or set `i18n.dir` explicitly.

- `messages.json` contains application, validation, bind, and HTTP error message keys, including QUERY's `content_type_required` guard.
- `fields.json` contains optional human-readable field labels.

The catalogs begin with namespaced, application-owned example keys. The English catalog then uses a single `_comment` entry to separate those examples from Credo's programmatic built-in defaults. The built-in entries are repeated so the file remains a complete, copyable override catalog; file entries override the programmatic base, but unchanged built-in entries may be removed individually—or as a group—when the built-in English fallback is sufficient. `_comment` is documentation only, appears in the English file only, and has no special loader semantics.

Credo does not add hidden prefixes to message keys. The supplied catalogs use bare framework codes; applications may adopt namespaces through explicit message keys or `I18nConfig.ResolveMessageKey`.

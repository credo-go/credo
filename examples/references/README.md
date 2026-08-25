# Credo reference files

This directory contains versioned, copyable reference files. Unlike the
`examples/hello` and `examples/saas` directories, it is not a runnable
application.

## Configuration

`config/config.yaml` and `config/config.json` describe the same configuration
in the two file formats supported by Credo. Copy one format into an application
root; using both is only useful when intentionally layering configuration,
because Credo merges all discovered config files in JSON, YAML, YML order.

- `server` is read automatically by `credo.New`.
- `i18n` is read when `app.UseI18n()` is called without an explicit file source.
- `databases` and `app` demonstrate application-owned typed snapshots. Read
  them at the composition root with `RawConfig.Unmarshal` or `GetConfig[T]`.
- Function-valued settings and programmatic options do not belong in these
  files.

The TLS paths are empty so the reference remains safe to run as plaintext.
Set both paths together to enable file-based TLS.

`config/.env.example` demonstrates environment overrides without containing
real credentials. Copy it to `.env` for local development; do not commit
secrets.

## Localization

`locales/` contains complete English and Turkish starter catalogs. Copy the
directory into an application root and either call `app.UseI18n()` for
conventional discovery or set `i18n.dir` explicitly.

- `messages.json` contains application, validation, bind, and HTTP error
  message keys.
- `fields.json` contains optional human-readable field labels.

Credo does not add hidden prefixes to message keys. The supplied catalogs use
bare framework codes; applications may adopt namespaces through explicit
message keys or `I18nConfig.ResolveMessageKey`.

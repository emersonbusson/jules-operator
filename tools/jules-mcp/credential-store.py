#!/usr/bin/env python3

"""Armazena a chave Jules no chaveiro de login sem expô-la em argumentos."""

import sys

import gi

gi.require_version("Secret", "1")
from gi.repository import Secret  # noqa: E402


SCHEMA = Secret.Schema.new(
    "com.ramshared.jules-operator",
    Secret.SchemaFlags.NONE,
    {"credential": Secret.SchemaAttributeType.STRING},
)
ATTRIBUTES = {"credential": "jules-api-key"}
LABEL = "RamShared Jules API key"


def fail(message: str) -> int:
    print(message, file=sys.stderr)
    return 2


def main() -> int:
    if len(sys.argv) != 2:
        return fail("Uso: credential-store.py read|write|status|delete")

    action = sys.argv[1]
    if action == "read":
        password = Secret.password_lookup_sync(SCHEMA, ATTRIBUTES, None)
        if password is None:
            return 2
        sys.stdout.write(password)
        return 0

    if action == "write":
        password = sys.stdin.read().strip()
        if not password:
            return fail("Credencial Jules vazia; o chaveiro não foi alterado.")
        stored = Secret.password_store_sync(
            SCHEMA,
            ATTRIBUTES,
            Secret.COLLECTION_DEFAULT,
            LABEL,
            password,
            None,
        )
        password = ""
        if not stored:
            return fail("O chaveiro de login recusou a credencial Jules.")
        return 0

    if action == "status":
        password = Secret.password_lookup_sync(SCHEMA, ATTRIBUTES, None)
        configured = password is not None
        password = ""
        print("configured" if configured else "missing")
        return 0 if configured else 1

    if action == "delete":
        Secret.password_clear_sync(SCHEMA, ATTRIBUTES, None)
        return 0

    return fail("Uso: credential-store.py read|write|status|delete")


if __name__ == "__main__":
    raise SystemExit(main())

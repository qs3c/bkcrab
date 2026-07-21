from __future__ import annotations

import pytest

from app.main import MIB, Settings


def test_settings_derive_parser_limits_from_canonical_rag_values(monkeypatch) -> None:
    for name in (
        "RAG_PARSER_MAX_INPUT_BYTES",
        "RAG_PARSER_MAX_OUTPUT_BYTES",
        "RAG_PARSER_PARSE_TIMEOUT_SECONDS",
    ):
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setenv("BKCRAB_RAG_LIMITS_MAX_FILE_MB", "17")
    monkeypatch.setenv("BKCRAB_RAG_LIMITS_MAX_EXTRACTED_BYTES", "1234567")
    monkeypatch.setenv("BKCRAB_RAG_LIMITS_PARSE_TIMEOUT_MS", "45000")

    settings = Settings.from_env()

    assert settings.max_input_bytes == 17 * MIB
    assert settings.max_output_bytes == 1234567
    assert settings.parse_timeout_seconds == 45


@pytest.mark.parametrize(
    ("canonical_name", "canonical_value", "alias_name", "alias_value"),
    [
        (
            "BKCRAB_RAG_LIMITS_MAX_FILE_MB",
            "50",
            "RAG_PARSER_MAX_INPUT_BYTES",
            "1",
        ),
        (
            "BKCRAB_RAG_LIMITS_MAX_EXTRACTED_BYTES",
            "209715200",
            "RAG_PARSER_MAX_OUTPUT_BYTES",
            "1",
        ),
        (
            "BKCRAB_RAG_LIMITS_PARSE_TIMEOUT_MS",
            "600000",
            "RAG_PARSER_PARSE_TIMEOUT_SECONDS",
            "1",
        ),
    ],
)
def test_settings_fail_fast_when_legacy_alias_drifts(
    monkeypatch,
    canonical_name: str,
    canonical_value: str,
    alias_name: str,
    alias_value: str,
) -> None:
    monkeypatch.setenv(canonical_name, canonical_value)
    monkeypatch.setenv(alias_name, alias_value)

    with pytest.raises(RuntimeError, match="must match the limit derived"):
        Settings.from_env()


def test_settings_round_canonical_timeout_up_to_parser_seconds(monkeypatch) -> None:
    monkeypatch.delenv("RAG_PARSER_PARSE_TIMEOUT_SECONDS", raising=False)
    monkeypatch.setenv("BKCRAB_RAG_LIMITS_PARSE_TIMEOUT_MS", "1501")

    assert Settings.from_env().parse_timeout_seconds == 2

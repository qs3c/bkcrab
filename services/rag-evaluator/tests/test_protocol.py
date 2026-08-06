import asyncio

import pytest
from pydantic import ValidationError

from app.metrics import MetricEngine
from app.protocol import EvaluateRequest, Sample


def test_protocol_rejects_unknown_metric_and_field():
    with pytest.raises(ValidationError):
        EvaluateRequest.model_validate({"requestId":"r","metricBundleVersion":"rag-core-v1","metrics":["exec_python"],"samples":[{"caseId":"c","userInput":"q","unknown":1}]})


def test_missing_metric_input_is_skipped_not_zero():
    result = asyncio.run(MetricEngine().evaluate("faithfulness", Sample(caseId="c", userInput="q", response="a")))
    assert result.status == "skipped_missing_input"
    assert result.value is None

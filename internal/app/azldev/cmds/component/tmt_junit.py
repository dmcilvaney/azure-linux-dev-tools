# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Export persisted results with pinned TMT, without waking or running any steps."""

from __future__ import annotations

# Embedded executable, not an importable package. Its dependencies live only in
# the per-work-directory TMT venv, never in the developer's Python environment.
# ruff: noqa: INP001
# pyright: reportMissingImports=false
import sys

import fmf
from tmt.base import Plan
from tmt.log import Logger
from tmt.result import Result
from tmt.steps.report.junit import ReportJUnit, ReportJUnitData, ResultsContext, make_junit_xml
from tmt.utils import Path, yaml_to_list


def _main() -> None:
    result_path, plan_name, output, template = sys.argv[1:]
    logger = Logger.create()
    parent = fmf.Tree({"summary": "JUnit export"})
    node = fmf.Tree({"execute": {"how": "tmt"}}, name=plan_name, parent=parent)
    plan = Plan(node=node, logger=logger, skip_validation=True)
    results = [Result.from_serialized(record) for record in yaml_to_list(Path(result_path).read_text(encoding="utf-8"))]
    reporter = ReportJUnit(step=plan.report, data=ReportJUnitData(name="azldev-junit", how="junit"), logger=logger)
    xml = make_junit_xml(
        reporter,
        flavor="custom",
        template_path=Path(template),
        include_output_log=False,
        results_context=ResultsContext(results),
    )
    Path(output).write_text(xml, encoding="utf-8")


if __name__ == "__main__":
    _main()

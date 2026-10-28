#!/usr/bin/env python3
"""Contract tests for the plugin-owned static command router."""

from __future__ import annotations

import json
import os
import pathlib
import shlex
import stat
import subprocess
import tempfile
import unittest


PLUGIN_ROOT = pathlib.Path(__file__).resolve().parents[1]
ROUTER = PLUGIN_ROOT / "bin" / "router.sh"


REPORTED_RG_CASES = [
    (
        "rg -l 'class HybridRouter|def select.*model|routing_policy' "
        "platform/services/llm/src | head",
        [
            "seek",
            "-n",
            "10",
            "-m",
            "3",
            'case:yes type:file content:"class HybridRouter|def select.*model|routing_policy"',
            "platform/services/llm/src",
        ],
    ),
    (
        "rg -n -C 4 'class HybridRouter|HybridRouter\\(' platform/services/llm/src",
        [
            "seek",
            "-n",
            "20",
            "-m",
            "1",
            "-C",
            "4",
            'case:yes content:"class HybridRouter|HybridRouter\\\\("',
            "platform/services/llm/src",
        ],
    ),
    (
        "rg -n -C 5 'routing_policy' "
        "platform/services/llm/src/duale_llm_service/handlers/message_processor.py "
        "platform/services/llm/src/duale_llm_service/services/manager.py",
        [
            "seek",
            "-n",
            "20",
            "-m",
            "1",
            "-C",
            "5",
            'case:yes content:"routing_policy"',
            "platform/services/llm/src/duale_llm_service/handlers/message_processor.py",
            "platform/services/llm/src/duale_llm_service/services/manager.py",
        ],
    ),
    (
        "rg -l 'class PublishingHandler|async def forward_llm_request|def forward_llm_request' "
        "platform/services/router/src",
        [
            "seek",
            "-n",
            "20",
            "-m",
            "3",
            'case:yes type:file content:"class PublishingHandler|async def forward_llm_request|def forward_llm_request"',
            "platform/services/router/src",
        ],
    ),
    (
        "rg -n -A 205 'class PublishingHandler' "
        "platform/services/router/src/duale_router_service/handlers/publishing_handler.py",
        [
            "seek",
            "-n",
            "20",
            "-m",
            "1",
            "-A",
            "205",
            'case:yes content:"class PublishingHandler"',
            "platform/services/router/src/duale_router_service/handlers/publishing_handler.py",
        ],
    ),
    (
        "rg -n -A 330 'async def _instantiate_and_route_agent' "
        "platform/services/router/src/duale_router_service/handlers/agent_routing_handler.py",
        [
            "seek",
            "-n",
            "20",
            "-m",
            "1",
            "-A",
            "330",
            'case:yes content:"async def _instantiate_and_route_agent"',
            "platform/services/router/src/duale_router_service/handlers/agent_routing_handler.py",
        ],
    ),
    (
        "rg -l 'class AgentRoutingHandler|route_to_agent_with_executor' "
        "platform/services/router/src",
        [
            "seek",
            "-n",
            "20",
            "-m",
            "3",
            'case:yes type:file content:"class AgentRoutingHandler|route_to_agent_with_executor"',
            "platform/services/router/src",
        ],
    ),
    (
        "rg -n -A 360 'class AgentRoutingHandler' "
        "platform/services/router/src/duale_router_service/handlers/agent_routing_handler.py",
        [
            "seek",
            "-n",
            "20",
            "-m",
            "1",
            "-A",
            "360",
            'case:yes content:"class AgentRoutingHandler"',
            "platform/services/router/src/duale_router_service/handlers/agent_routing_handler.py",
        ],
    ),
    (
        "rg -l 'class RouteExecutionHandler|async def execute_route_with_span' "
        "platform/services/router/src",
        [
            "seek",
            "-n",
            "20",
            "-m",
            "3",
            'case:yes type:file content:"class RouteExecutionHandler|async def execute_route_with_span"',
            "platform/services/router/src",
        ],
    ),
    (
        "rg -n -A 330 'class RouteExecutionHandler' "
        "platform/services/router/src/duale_router_service/handlers/route_execution_handler.py",
        [
            "seek",
            "-n",
            "20",
            "-m",
            "1",
            "-A",
            "330",
            'case:yes content:"class RouteExecutionHandler"',
            "platform/services/router/src/duale_router_service/handlers/route_execution_handler.py",
        ],
    ),
    (
        "rg -n -C 12 'TaskHandler\\(|make_routing_decision_callback|execute_route_callback|"
        "async def execute_route|def execute_route' "
        "platform/services/router/src/duale_router_service/core/core_router.py",
        [
            "seek",
            "-n",
            "20",
            "-m",
            "1",
            "-C",
            "12",
            'case:yes content:"TaskHandler\\\\(|make_routing_decision_callback|execute_route_callback|async def execute_route|def execute_route"',
            "platform/services/router/src/duale_router_service/core/core_router.py",
        ],
    ),
    (
        "rg -l 'def make_routing_decision|async def make_routing_decision|"
        "make_routing_decision_callback' platform/services/router/src",
        [
            "seek",
            "-n",
            "20",
            "-m",
            "3",
            'case:yes type:file content:"def make_routing_decision|async def make_routing_decision|make_routing_decision_callback"',
            "platform/services/router/src",
        ],
    ),
    (
        "rg -n -A 240 'class RoutingDecisionHandler|async def make_routing_decision' "
        "platform/services/router/src/duale_router_service/handlers/routing_decision_handler.py",
        [
            "seek",
            "-n",
            "20",
            "-m",
            "1",
            "-A",
            "240",
            'case:yes content:"class RoutingDecisionHandler|async def make_routing_decision"',
            "platform/services/router/src/duale_router_service/handlers/routing_decision_handler.py",
        ],
    ),
    (
        "rg -n -A 162 'async def _create_root_persistence_chain' "
        "platform/services/router/src/duale_router_service/handlers/task_handler.py",
        [
            "seek",
            "-n",
            "20",
            "-m",
            "1",
            "-A",
            "162",
            'case:yes content:"async def _create_root_persistence_chain"',
            "platform/services/router/src/duale_router_service/handlers/task_handler.py",
        ],
    ),
    (
        "rg -n -A 125 'async def _ensure_route_persisted' "
        "platform/services/router/src/duale_router_service/handlers/task_handler.py",
        [
            "seek",
            "-n",
            "20",
            "-m",
            "1",
            "-A",
            "125",
            'case:yes content:"async def _ensure_route_persisted"',
            "platform/services/router/src/duale_router_service/handlers/task_handler.py",
        ],
    ),
    (
        "rg -n -A 125 'async def _resolve_route_plan' "
        "platform/services/router/src/duale_router_service/handlers/task_handler.py",
        [
            "seek",
            "-n",
            "20",
            "-m",
            "1",
            "-A",
            "125",
            'case:yes content:"async def _resolve_route_plan"',
            "platform/services/router/src/duale_router_service/handlers/task_handler.py",
        ],
    ),
    (
        "rg -n -A 190 'class BridgeTaskRequestHandler' "
        "platform/services/router/src/duale_router_service/core/typed_handlers.py",
        [
            "seek",
            "-n",
            "20",
            "-m",
            "1",
            "-A",
            "190",
            'case:yes content:"class BridgeTaskRequestHandler"',
            "platform/services/router/src/duale_router_service/core/typed_handlers.py",
        ],
    ),
    (
        "rg -n -A 180 'async def setup_typed_consumers' "
        "platform/services/router/src/duale_router_service/main.py",
        [
            "seek",
            "-n",
            "20",
            "-m",
            "1",
            "-A",
            "180",
            'case:yes content:"async def setup_typed_consumers"',
            "platform/services/router/src/duale_router_service/main.py",
        ],
    ),
]


class RouterContractTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.temp_dir = tempfile.TemporaryDirectory()
        cls.bin_dir = pathlib.Path(cls.temp_dir.name)
        cls.seek_stub = cls.bin_dir / "seek"
        cls._write_seek_stub()
        cls.env = os.environ.copy()
        cls.env["PATH"] = f"{cls.bin_dir}{os.pathsep}{cls.env['PATH']}"

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp_dir.cleanup()

    @classmethod
    def _write_seek_stub(cls) -> None:
        cls.seek_stub.write_text(
            "#!/bin/sh\n"
            "exit 0\n",
            encoding="utf-8",
        )
        cls.seek_stub.chmod(cls.seek_stub.stat().st_mode | stat.S_IXUSR)

    def run_raw(self, payload: str, env: dict[str, str] | None = None) -> str:
        result = subprocess.run(
            ["/bin/sh", str(ROUTER)],
            input=payload,
            text=True,
            capture_output=True,
            timeout=5,
            env=env or self.env,
            check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stderr, "")
        return result.stdout.strip()

    def run_command(
        self,
        command: str,
        env: dict[str, str] | None = None,
        **fields: object,
    ) -> str:
        tool_input: dict[str, object] = {"command": command}
        tool_input.update(fields)
        payload = json.dumps({"tool_name": "Bash", "tool_input": tool_input})
        return self.run_raw(payload, env)

    def routed(
        self,
        command: str,
        env: dict[str, str] | None = None,
        **fields: object,
    ) -> tuple[list[str], dict[str, object]]:
        output = self.run_command(command, env, **fields)
        self.assertNotEqual(output, "", command)
        response = json.loads(output)
        self.assertEqual(set(response), {"hookSpecificOutput"})
        hook = response["hookSpecificOutput"]
        self.assertEqual(
            set(hook), {"hookEventName", "permissionDecision", "updatedInput"}
        )
        self.assertEqual(hook["hookEventName"], "PreToolUse")
        self.assertEqual(hook["permissionDecision"], "allow")
        return shlex.split(hook["updatedInput"]["command"]), hook["updatedInput"]

    def assert_passthrough(self, command: str) -> None:
        self.assertEqual(self.run_command(command), "", command)

    def test_all_reported_rg_commands_route_exactly(self) -> None:
        self.assertEqual(len(REPORTED_RG_CASES), 18)
        for command, expected in REPORTED_RG_CASES:
            with self.subTest(command=command):
                actual, _ = self.routed(command)
                self.assertEqual(actual, expected)

    def test_adapter_contracts(self) -> None:
        cases = [
            (
                "grep -rniHIF Foo ./cmd",
                ["seek", "-n", "20", "-m", "3", 'case:no content:"Foo"', "./cmd"],
            ),
            (
                "rg -niHIF Foo ./cmd",
                ["seek", "-n", "20", "-m", "3", 'case:no content:"Foo"', "./cmd"],
            ),
            (
                "git grep -nil needle -- cmd plugins",
                [
                    "seek",
                    "-n",
                    "20",
                    "-m",
                    "3",
                    'case:no type:file content:"needle"',
                    "cmd",
                    "plugins",
                ],
            ),
            (
                "fd -t f Router ./cmd",
                [
                    "seek",
                    "-n",
                    "20",
                    "-m",
                    "3",
                    'case:yes type:file file:"(^|/)[^/]*Router[^/]*$"',
                    "./cmd",
                ],
            ),
            (
                "fd 'Étage' .",
                ["seek", "-n", "20", "-m", "3", 'case:no type:file file:"(^|/)[^/]*Étage[^/]*$"', "."],
            ),
            (
                # A literal name stays inside the file name: the root text
                # that the header holds does not match it.
                "fd sub ./sub",
                ["seek", "-n", "20", "-m", "3", 'case:no type:file file:"(^|/)[^/]*sub[^/]*$"', "./sub"],
            ),
            (
                "grep -rl needle ./cmd | head -5",
                ["seek", "-n", "5", "-m", "3", 'case:yes type:file content:"needle"', "./cmd"],
            ),
            (
                "rg -e needle ./cmd",
                ["seek", "-n", "20", "-m", "3", 'case:yes content:"needle"', "./cmd"],
            ),
            (
                "rg --regexp=needle ./cmd",
                ["seek", "-n", "20", "-m", "3", 'case:yes content:"needle"', "./cmd"],
            ),
            (
                "rg -inerouter ./cmd",
                ["seek", "-n", "20", "-m", "3", 'case:no content:"router"', "./cmd"],
            ),
            (
                "rg --no-filename needle ./cmd",
                ["seek", "-n", "20", "-m", "3", 'case:yes content:"needle"', "./cmd"],
            ),
            (
                "rg --fixed-strings 'a.b' ./cmd",
                ["seek", "-n", "20", "-m", "3", 'case:yes content:"a\\\\.b"', "./cmd"],
            ),
            (
                "rg -nA5 needle file.go",
                ["seek", "-n", "20", "-m", "1", "-A", "5", 'case:yes content:"needle"', "file.go"],
            ),
            (
                "rg --context=7 needle file.go",
                ["seek", "-n", "20", "-m", "1", "-C", "7", 'case:yes content:"needle"', "file.go"],
            ),
            (
                "rg --after-context 6 needle file.go",
                ["seek", "-n", "20", "-m", "1", "-A", "6", 'case:yes content:"needle"', "file.go"],
            ),
            (
                "git grep -F 'a.b' -- cmd",
                ["seek", "-n", "20", "-m", "3", 'case:yes content:"a\\\\.b"', "cmd"],
            ),
            (
                "find . -type f -name '*.go' -print",
                [
                    "seek",
                    "-n",
                    "20",
                    "-m",
                    "3",
                    'case:yes type:file file:"(^|/)[^/]*\\\\.go$"',
                    ".",
                ],
            ),
            (
                "find . -name '*.go' -type f",
                [
                    "seek",
                    "-n",
                    "20",
                    "-m",
                    "3",
                    'case:yes type:file file:"(^|/)[^/]*\\\\.go$"',
                    ".",
                ],
            ),
        ]
        for command, expected in cases:
            with self.subTest(command=command):
                actual, _ = self.routed(command)
                self.assertEqual(actual, expected)

    def test_file_head_limit_forms(self) -> None:
        cases = {
            "rg -l needle . | head": "10",
            "rg -l needle . | head -7": "7",
            "rg -l needle . | head -n 7": "7",
            "rg -l needle . | head -n7": "7",
            "rg -l needle . | head --lines 7": "7",
            "rg -l needle . | head --lines=7": "7",
        }
        for command, expected_limit in cases.items():
            with self.subTest(command=command):
                actual, _ = self.routed(command)
                self.assertEqual(actual[:3], ["seek", "-n", expected_limit])

    def test_complete_input_is_preserved(self) -> None:
        fields = {
            "description": "café",
            "timeout": 5000,
            "run_in_background": False,
            "custom": {"keep": True},
        }
        _, updated = self.routed("grep -rniHIF Foo 'path with space'", **fields)
        self.assertEqual(updated, {"command": updated["command"], **fields})

    def test_shell_quoting_preserves_static_values(self) -> None:
        actual, _ = self.routed("rg -F \"café's\" \"path with space\"")
        self.assertEqual(
            actual,
            [
                "seek",
                "-n",
                "20",
                "-m",
                "3",
                'case:yes content:"café\'s"',
                "path with space",
            ],
        )

    def test_context_has_no_router_cap(self) -> None:
        # Seek has no context cap, so the router keeps every context size that
        # Seek reads (an int64), as decimal text.
        cases = {
            "rg -C 513 needle file.go": "513",
            "rg -C 007 needle file.go": "7",
            "rg -A 9223372036854775807 needle file.go": "9223372036854775807",
        }
        for command, lines in cases.items():
            with self.subTest(command=command):
                actual, _ = self.routed(command)
                self.assertEqual(actual[5:7], [command.split()[1], lines])
        for command in [
            "rg -C 9223372036854775808 needle file.go",
            "rg -C 99999999999999999999 needle file.go",
        ]:
            with self.subTest(command=command):
                self.assert_passthrough(command)

    def test_safe_pathless_forms_stay_in_current_directory(self) -> None:
        # Seek searches the current directory when it gets no path, so the
        # rewrite adds none.
        for command in ["rg needle", "git grep needle", "fd needle"]:
            with self.subTest(command=command):
                actual, _ = self.routed(command)
                self.assertNotIn(".", actual)
                self.assertIn("needle", actual[-1])

    def test_unsupported_shell_forms_pass_through(self) -> None:
        commands = [
            'rg "$PATTERN" .',
            'rg "$(get-pattern)" .',
            "MODE=x rg needle .",
            "rg needle . 2>/dev/null",
            "rg needle . &",
            "rg needle .; echo done",
            "rg needle . && echo done",
            "rg -l needle . | sort",
            "rg -l needle . | head | sort",
            "rg -l needle . | head -n +7",
            "rg -l needle . | head -n 0",
            "rg -l needle . | head --lines=0",
            "rg needle *.go",
            "rg needle ~/src",
            "rg needle {src,test}",
            "rg needle . # locate",
            "rg 'needle .",
            "rg needle .\necho done",
        ]
        for command in commands:
            with self.subTest(command=command):
                self.assert_passthrough(command)

    def test_unsupported_tool_forms_pass_through(self) -> None:
        commands = [
            "grep needle",
            "grep -n needle .",
            "grep -rFE 'seek.router' README.md",
            "grep -rEF 'seek.router' README.md",
            "rg -P needle .",
            "rg --json needle .",
            "rg -C 4 needle",
            "rg -l -C 4 needle file.go",
            "git grep 'foo|bar'",
            "git grep needle HEAD",
            "git grep needle -- ':*.go'",
            "fd -e go needle .",
            # A regex pattern stays with fd: the router routes literal names.
            "fd 'main.*go' ./mainpkg",
            "fd '\\.go$' .",
            "fd '^a|b$' ./sub",
            "fd sub/x .",
            "find . ! -type f -name '*.go'",
            "find . -type f -name '*.go' -exec echo {} ';'",
        ]
        for command in commands:
            with self.subTest(command=command):
                self.assert_passthrough(command)

    def test_malformed_and_other_tool_payloads_fail_open(self) -> None:
        payloads = [
            "",
            "hello",
            '{"tool_name":',
            '{"tool_name":"Bash"}',
            '{"tool_name":"Grep","tool_input":{"pattern":"needle"}}',
            '{"tool_name":"Bash","tool_input":{"command":7}}',
        ]
        for payload in payloads:
            with self.subTest(payload=payload):
                self.assertEqual(self.run_raw(payload), "")

    def test_bypass_forms_fail_open(self) -> None:
        self.assert_passthrough("SEEK_ROUTER=off rg needle .")
        env = self.env.copy()
        env["SEEK_ROUTER"] = "off"
        payload = json.dumps(
            {"tool_name": "Bash", "tool_input": {"command": "rg needle ."}}
        )
        self.assertEqual(self.run_raw(payload, env), "")

    def test_router_uses_its_documented_regex_and_case_contract(self) -> None:
        # The router does not read a ripgrep configuration file. These flags
        # would make each search case-insensitive and literal.
        config = self.bin_dir / "rg-config"
        config.write_text("--ignore-case\n--fixed-strings\n", encoding="utf-8")
        configured = self.env.copy()
        configured["RIPGREP_CONFIG_PATH"] = str(config)
        cases = {
            "rg '(?s)foo.*bar' .": 'case:yes content:"(?s)foo.*bar"',
            "rg 'caf\\w' .": 'case:yes content:"caf\\\\w"',
            "rg SUPPORTED .": 'case:yes content:"SUPPORTED"',
        }
        for command, expected_query in cases.items():
            with self.subTest(command=command):
                actual, _ = self.routed(command, configured)
                self.assertEqual(actual[5], expected_query)

if __name__ == "__main__":
    unittest.main(verbosity=2)

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
REPO_ROOT = PLUGIN_ROOT.parents[1]
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
        cls._write_seek_stub("-A, --after-context\n-C, --context\n")
        cls.env = os.environ.copy()
        cls.env["PATH"] = f"{cls.bin_dir}{os.pathsep}{cls.env['PATH']}"

    @classmethod
    def tearDownClass(cls) -> None:
        cls.temp_dir.cleanup()

    @classmethod
    def _write_seek_stub(cls, help_text: str) -> None:
        cls.seek_stub.write_text(
            "#!/bin/sh\n"
            'if [ -n "${SEEK_TEST_REQUIRED_CACHE:-}" ] '
            '&& [ "${SEEK_CACHE_DIR:-}" != "$SEEK_TEST_REQUIRED_CACHE" ]; then\n'
            "  exit 1\n"
            "fi\n"
            'if [ "${1:-}" = "--help" ]; then\n'
            f"  printf '%s' {shlex.quote(help_text)}\n"
            "fi\n"
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
                    'case:yes type:file file:"Router"',
                    "./cmd",
                ],
            ),
            (
                "fd 'Étage' .",
                ["seek", "-n", "20", "-m", "3", 'case:no type:file file:"Étage"', "."],
            ),
            (
                "fd '\\Acargo' .",
                ["seek", "-n", "20", "-m", "3", 'case:yes type:file file:"\\\\Acargo"', "."],
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

    def test_safe_pathless_forms_stay_in_current_directory(self) -> None:
        cases = {
            "rg needle": ["."],
            "git grep needle": ["."],
            "fd needle": ["."],
        }
        for command, suffix in cases.items():
            with self.subTest(command=command):
                actual, _ = self.routed(command)
                self.assertEqual(actual[-len(suffix) :], suffix)

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
            "rg -C 513 needle file.go",
            "rg -C 4 needle",
            "rg -l -C 4 needle file.go",
            "git grep 'foo|bar'",
            "git grep needle HEAD",
            "git grep needle -- ':*.go'",
            "fd -e go needle .",
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

    def test_old_seek_only_blocks_context_routes(self) -> None:
        self._write_seek_stub("seek help without context flags\n")
        try:
            self.assert_passthrough("rg -A 30 needle file.go")
            commands = [
                "rg -l needle .",
                "rg -F 'foo -A bar' file.go",
                "rg needle 'path -C segment'",
            ]
            for command in commands:
                with self.subTest(command=command):
                    actual, _ = self.routed(command)
                    self.assertEqual(actual[0], "seek")
        finally:
            self._write_seek_stub("-A, --after-context\n-C, --context\n")

    def test_context_probe_preserves_search_cache(self) -> None:
        env = self.env.copy()
        cache = str(self.bin_dir / "real-cache")
        env["SEEK_CACHE_DIR"] = cache
        env["SEEK_TEST_REQUIRED_CACHE"] = cache
        actual, _ = self.routed("rg -A 5 needle file.go", env)
        self.assertEqual(actual[0], "seek")

    def test_router_uses_its_documented_regex_and_case_contract(self) -> None:
        configured = self.env.copy()
        configured["RIPGREP_CONFIG_PATH"] = str(self.bin_dir / "rg-config")
        cases = {
            "rg '(?s)foo.*bar' .": 'case:yes content:"(?s)foo.*bar"',
            "rg 'caf\\w' .": 'case:yes content:"caf\\\\w"',
            "rg SUPPORTED .": 'case:yes content:"SUPPORTED"',
        }
        for command, expected_query in cases.items():
            with self.subTest(command=command):
                actual, _ = self.routed(command, configured)
                self.assertEqual(actual[5], expected_query)

    def test_router_implementation_is_outside_seek(self) -> None:
        self.assertFalse((REPO_ROOT / "cmd" / "seek" / "hook_router.go").exists())
        main_source = (REPO_ROOT / "cmd" / "seek" / "main.go").read_text(encoding="utf-8")
        module = (REPO_ROOT / "go.mod").read_text(encoding="utf-8")
        seek_binary = REPO_ROOT / "seek"
        self.assertTrue(seek_binary.is_file())
        result = subprocess.run(
            [str(seek_binary), "--hook-route"],
            text=True,
            capture_output=True,
            timeout=5,
            check=False,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unknown flag: --hook-route", result.stderr)
        self.assertNotIn("hook-route", main_source)
        self.assertNotIn("routeHookPayload", main_source)
        self.assertNotIn("mvdan.cc/sh", module)
        self.assertTrue((PLUGIN_ROOT / "lib" / "router.awk").is_file())


if __name__ == "__main__":
    unittest.main(verbosity=2)

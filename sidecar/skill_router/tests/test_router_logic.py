import json

from scripts.skill_router_sidecar import (
    RouterConfig,
    SkillRouterService,
    extract_symbols,
    infer_weather_location_from_text,
    plan_route,
    render_status_page_html,
)


def test_extract_symbols_normalizes_market_suffix():
    symbols = extract_symbols("quote TSLA and 700.hk")
    assert "TSLA.US" in symbols
    assert "700.HK" in symbols


def test_plan_route_longbridge_quote():
    intent, arguments, actions = plan_route("longbridge", "give me TSLA price")
    assert intent == "get_stock_price"
    assert actions
    assert actions[0]["type"] == "lb"


def test_plan_route_pdf_extract():
    intent, arguments, actions = plan_route("pdf", "pdftotext report.pdf out.txt")
    assert intent == "pdf_to_text"
    assert arguments["input_file"].endswith(".pdf")
    assert actions[0]["type"] in {"task", "sys"}


def test_infer_weather_location_from_text_extracts_city_without_hardcode():
    city = infer_weather_location_from_text("can you show weather in Singapore today")
    assert city.lower().startswith("singapore")


def test_infer_weather_location_from_mixed_text_extracts_city():
    city = infer_weather_location_from_text("当前北京的天气 特斯拉股票的价格 打印到终端")
    assert city == "北京"


def test_infer_weather_location_from_quote_plus_weather_extracts_weather_city():
    city = infer_weather_location_from_text("特斯拉股票价格 上海天气 保存到pdf文件")
    assert city == "上海"


def test_status_payload_exposes_service_health():
    service = SkillRouterService(
        RouterConfig(
            auth_token="",
            llm_api_key="",
            llm_base_url="",
            llm_model="gpt-4.1-mini",
        )
    )
    status = service.status_payload()
    assert status["status"] == "ok"
    assert status["router"]["requests_total"] == 0
    assert status["endpoints"]["status_page"] == "/status"


def test_status_payload_updates_after_route():
    service = SkillRouterService(
        RouterConfig(
            auth_token="",
            llm_api_key="",
            llm_base_url="",
            llm_model="gpt-4.1-mini",
        )
    )
    result = service.route(
        {
            "request_id": "req-test",
            "text": "上海天气",
            "installed_skills": [{"id": "weather"}],
            "runtime_context": {"runtime": "test"},
        }
    )
    status = service.status_payload()
    assert status["router"]["requests_total"] >= 1
    assert status["router"]["last_intent"] == result["intent"]
    html = render_status_page_html(status)
    assert "Longtrade Skill Router Status" in html


def test_route_prefers_longbridge_for_chinese_market_request():
    service = SkillRouterService(
        RouterConfig(
            auth_token="",
            llm_api_key="",
            llm_base_url="",
            llm_model="gpt-4.1-mini",
        )
    )
    result = service.route(
        {
            "request_id": "req-tsla-cn",
            "text": "特斯拉股票价格 使用longbridge获取",
            "installed_skills": [
                {"id": "find-skills"},
                {"id": "longbridge"},
                {"id": "weather"},
                {"id": "pdf"},
            ],
            "runtime_context": {"runtime": "test"},
        }
    )
    assert result["skill_id"] == "longbridge"
    assert result["intent"] == "get_stock_price"
    assert float(result["confidence"]) >= 0.8
    assert result["actions"]
    assert result["actions"][0]["type"] == "lb"
    assert "TSLA.US" not in result["actions"][0]["command"]


def test_route_handles_skill_inventory_query_with_find_skills():
    service = SkillRouterService(
        RouterConfig(
            auth_token="",
            llm_api_key="",
            llm_base_url="",
            llm_model="gpt-4.1-mini",
        )
    )
    result = service.route(
        {
            "request_id": "req-skills-count",
            "text": "当前支持的skills有几个",
            "installed_skills": [
                {"id": "find-skills"},
                {"id": "longbridge"},
                {"id": "weather"},
                {"id": "pdf"},
            ],
            "runtime_context": {"runtime": "test"},
        }
    )
    assert result["skill_id"] == "find-skills"
    assert result["intent"] == "list_supported_skills"
    assert float(result["confidence"]) >= 0.8
    assert result["actions"]
    assert result["actions"][0]["type"] == "sys"
    assert "supported_skills_count=4" in result["actions"][0]["command"]


class _FakeLLMResponse:
    def __init__(self, content: str):
        self.content = content


class _FakeLLM:
    def __init__(self, content: str):
        self.content = content
        self.last_prompt = ""

    def invoke(self, prompt: str):
        self.last_prompt = prompt
        return _FakeLLMResponse(self.content)


def test_route_with_llm_prompt_does_not_force_pdf_constraints():
    service = SkillRouterService(
        RouterConfig(
            auth_token="",
            llm_api_key="dummy-key",
            llm_base_url="https://api.example.com/v1",
            llm_model="gpt-4.1-mini",
        )
    )
    fake_llm = _FakeLLM(
        json.dumps(
            {
                "skill_id": "multi-skill",
                "intent": "multi_skill_workflow",
                "arguments": {"output_file": "testit.pdf"},
                "confidence": 0.9,
                "steps": [
                    {
                        "skill_id": "longbridge",
                        "intent": "get_stock_price",
                        "arguments": {"symbols": ["TSLA.US", "AAPL.US"]},
                        "actions": [{"type": "lb", "command": "quote TSLA.US AAPL.US --format json"}],
                        "reason": "collect quotes",
                    },
                    {
                        "skill_id": "pdf",
                        "intent": "generate_pdf",
                        "arguments": {"output_file": "testit.pdf"},
                        "actions": [{"type": "sys", "command": "python3 render_pdf.py testit.pdf"}],
                        "reason": "render pdf",
                    },
                ],
            }
        )
    )
    service.llm = fake_llm
    req = {
        "text": "获取伦敦的天气 特斯拉苹果的股票价格 保存到pdf文件 文件名testit.pdf",
        "installed_skills": [
            {"id": "longbridge", "allowed_actions": ["lb", "sys"]},
            {"id": "pdf", "allowed_actions": ["sys"]},
            {"id": "weather", "allowed_actions": ["sys"]},
        ],
    }
    route = service._route_with_llm(req, entity_hints={})
    assert route["skill_id"] == "multi-skill"
    assert "save/export/generate PDF" not in fake_llm.last_prompt
    assert "steps must include one pdf step with non-empty actions" not in fake_llm.last_prompt
    assert "Never invent unavailable commands or skill ids." in fake_llm.last_prompt
    assert "获取伦敦的天气 特斯拉苹果的股票价格 保存到pdf文件 文件名testit.pdf" in fake_llm.last_prompt


def test_route_prefers_llm_steps_for_weather_quote_pdf_text():
    service = SkillRouterService(
        RouterConfig(
            auth_token="",
            llm_api_key="dummy-key",
            llm_base_url="https://api.example.com/v1",
            llm_model="gpt-4.1-mini",
        )
    )
    fake_llm = _FakeLLM(
        json.dumps(
            {
                "skill_id": "multi-skill",
                "intent": "multi_skill_workflow",
                "arguments": {"output_file": "testit.pdf"},
                "confidence": 0.93,
                "steps": [
                    {
                        "skill_id": "weather",
                        "intent": "get_weather",
                        "arguments": {"location": "London"},
                        "actions": [{"type": "sys", "command": "curl -s \"wttr.in/London?format=3\""}],
                        "reason": "collect weather",
                    },
                    {
                        "skill_id": "longbridge",
                        "intent": "get_stock_price",
                        "arguments": {"symbols": ["TSLA.US", "AAPL.US"]},
                        "actions": [{"type": "lb", "command": "quote TSLA.US AAPL.US --format json"}],
                        "reason": "collect quotes",
                    },
                    {
                        "skill_id": "pdf",
                        "intent": "generate_pdf",
                        "arguments": {"output_file": "testit.pdf"},
                        "actions": [{"type": "sys", "command": "python3 render_pdf.py testit.pdf"}],
                        "reason": "render pdf",
                    },
                ],
            }
        )
    )
    service.llm = fake_llm
    result = service.route(
        {
            "request_id": "req-weather-quote-pdf",
            "text": "获取伦敦的天气 特斯拉苹果的股票价格 保存到pdf文件 文件名testit.pdf",
            "installed_skills": [
                {"id": "find-skills"},
                {"id": "longbridge"},
                {"id": "weather"},
                {"id": "pdf"},
            ],
            "runtime_context": {"runtime": "test"},
        }
    )
    assert result["skill_id"] == "multi-skill"
    assert result["intent"] == "multi_skill_workflow"
    assert len(result["steps"]) == 3
    assert result["steps"][2]["skill_id"] == "pdf"


def test_route_recommendations_avoid_false_positive_when_current_skill_matches():
    service = SkillRouterService(
        RouterConfig(
            auth_token="",
            llm_api_key="",
            llm_base_url="",
            llm_model="gpt-4.1-mini",
        )
    )
    result = service.route(
        {
            "request_id": "req-pdf-match",
            "text": "写入 我们明天到公园玩 到pdf文件",
            "installed_skills": [{"id": "pdf", "description": "PDF generation and extraction"}],
            "available_skills": [
                {
                    "id": "pdf",
                    "capabilities": ["pdf"],
                    "priority": 50,
                    "source": "anthropics/skills",
                    "install_command_template": "npx skills add {source} -g -y",
                }
            ],
        }
    )
    assert result["skill_id"] == "pdf"
    assert result["recommendations"] == []


def test_route_recommendations_include_missing_capability():
    service = SkillRouterService(
        RouterConfig(
            auth_token="",
            llm_api_key="",
            llm_base_url="",
            llm_model="gpt-4.1-mini",
        )
    )
    result = service.route(
        {
            "request_id": "req-missing-weather",
            "text": "上海天气怎么样",
            "installed_skills": [{"id": "pdf", "description": "PDF generation and extraction"}],
            "available_skills": [
                {
                    "id": "weather-pro",
                    "capabilities": ["weather"],
                    "priority": 80,
                    "source": "sample/weather-pro",
                    "install_command_template": "npx skills add {source} -g -y",
                }
            ],
        }
    )
    assert result["skill_id"] == "pdf"
    assert len(result["recommendations"]) == 1
    assert result["recommendations"][0]["skill_id"] == "weather-pro"
    assert result["recommendations"][0]["reason"] == "missing_capability"
    assert result["recommendations"][0]["install_command"] == "npx skills add sample/weather-pro -g -y"


def test_route_recommendations_include_better_match_available():
    service = SkillRouterService(
        RouterConfig(
            auth_token="",
            llm_api_key="",
            llm_base_url="",
            llm_model="gpt-4.1-mini",
        )
    )
    result = service.route(
        {
            "request_id": "req-better-pdf",
            "text": "生成一个pdf文件 内容是hello",
            "installed_skills": [{"id": "pdf", "description": "PDF generation and extraction"}],
            "available_skills": [
                {
                    "id": "pdf",
                    "capabilities": ["pdf"],
                    "priority": 50,
                    "source": "anthropics/skills",
                    "install_command_template": "npx skills add {source} -g -y",
                },
                {
                    "id": "pdf-pro",
                    "capabilities": ["pdf"],
                    "priority": 90,
                    "source": "sample/pdf-pro",
                    "install_command_template": "npx skills add {source} -g -y",
                },
            ],
        }
    )
    assert result["skill_id"] == "pdf"
    assert len(result["recommendations"]) == 1
    assert result["recommendations"][0]["skill_id"] == "pdf-pro"
    assert result["recommendations"][0]["reason"] == "better_match_available"


def test_route_handles_basic_arithmetic_with_builtin_fallback():
    service = SkillRouterService(
        RouterConfig(
            auth_token="",
            llm_api_key="",
            llm_base_url="",
            llm_model="gpt-4.1-mini",
        )
    )
    result = service.route(
        {
            "request_id": "req-basic-math",
            "text": "可以帮我计算下1+1=多少",
            "installed_skills": [
                {"id": "find-skills", "allowed_actions": ["sys"]},
                {"id": "pdf", "allowed_actions": ["sys", "task"]},
            ],
        }
    )
    assert result["skill_id"] == "find-skills"
    assert result["intent"] == "basic_arithmetic"
    assert result["arguments"]["expression"] == "1+1"
    assert result["arguments"]["result"] == "2"
    assert float(result["confidence"]) >= 0.9
    assert result["actions"]
    assert result["actions"][0]["type"] == "sys"
    assert "2" in result["actions"][0]["command"]


def test_route_recommendations_include_missing_calculation_capability():
    service = SkillRouterService(
        RouterConfig(
            auth_token="",
            llm_api_key="",
            llm_base_url="",
            llm_model="gpt-4.1-mini",
        )
    )
    result = service.route(
        {
            "request_id": "req-math-recommend",
            "text": "帮我算一下 12*(5+3)",
            "installed_skills": [{"id": "find-skills", "description": "Skill discovery helper", "allowed_actions": ["sys"]}],
            "available_skills": [
                {
                    "id": "calculator-pro",
                    "capabilities": ["calculation"],
                    "priority": 95,
                    "source": "sample/calculator-pro",
                    "install_command_template": "npx skills add {source} -g -y",
                }
            ],
        }
    )
    assert result["intent"] == "basic_arithmetic"
    assert len(result["recommendations"]) == 1
    assert result["recommendations"][0]["skill_id"] == "calculator-pro"
    assert result["recommendations"][0]["reason"] == "missing_capability"


def test_route_prefers_installed_calculator_for_arithmetic_and_avoids_duplicate_recommendation():
    service = SkillRouterService(
        RouterConfig(
            auth_token="",
            llm_api_key="",
            llm_base_url="",
            llm_model="gpt-4.1-mini",
        )
    )
    result = service.route(
        {
            "request_id": "req-installed-calc",
            "text": "1+3= 多少",
            "installed_skills": [
                {"id": "find-skills", "description": "Skill discovery helper", "allowed_actions": ["sys"]},
                {"id": "tam-sam-som-calculator", "description": "calculator utilities", "allowed_actions": ["sys"]},
            ],
            "available_skills": [
                {
                    "id": "calculator",
                    "capabilities": ["calculation"],
                    "priority": 70,
                    "source": "openai/skills@calculator",
                    "install_command_template": "npx skills add {source} -g -y",
                }
            ],
        }
    )
    assert result["skill_id"] == "tam-sam-som-calculator"
    assert result["intent"] == "basic_arithmetic"
    assert result["arguments"]["result"] == "4"
    assert result["recommendations"] == []


def test_route_arithmetic_pdf_request_prefers_pdf_generation_with_computed_content():
    service = SkillRouterService(
        RouterConfig(
            auth_token="",
            llm_api_key="",
            llm_base_url="",
            llm_model="gpt-4.1-mini",
        )
    )
    result = service.route(
        {
            "request_id": "req-arithmetic-pdf",
            "text": "7*8= save result to file xxxy.pdf",
            "installed_skills": [
                {"id": "pdf", "description": "PDF generation and extraction", "allowed_actions": ["sys", "task"]},
                {"id": "tam-sam-som-calculator", "description": "calculator utilities", "allowed_actions": ["sys"]},
            ],
        }
    )
    assert result["skill_id"] == "pdf"
    assert result["intent"] == "generate_pdf"
    assert result["arguments"]["content"] == "56"
    assert result["actions"]
    assert result["actions"][0]["type"] == "task"
    assert "56" in result["actions"][0]["command"]


def test_enforce_requirements_retries_when_route_misses_pdf_output():
    service = SkillRouterService(
        RouterConfig(
            auth_token="",
            llm_api_key="",
            llm_base_url="",
            llm_model="gpt-4.1-mini",
        )
    )
    state = {
        "request": {
            "text": "7*8= save result to file xxxy.pdf",
            "installed_skills": [
                {"id": "pdf", "description": "PDF generation and extraction", "allowed_actions": ["sys", "task"]},
                {"id": "find-skills", "allowed_actions": ["sys"]},
            ],
        },
        "basic_arithmetic": {"expression": "7*8", "result": "56"},
        "selected_skill_id": "find-skills",
        "selected_intent": "basic_arithmetic",
        "selected_arguments": {"expression": "7*8", "result": "56"},
        "selected_actions": [{"type": "sys", "command": "echo 56"}],
        "selected_steps": [],
        "confidence": 0.9,
    }
    result = service.enforce_requirements(state)
    assert result["selected_skill_id"] == "pdf"
    assert result["selected_intent"] == "generate_pdf"
    assert result["selected_arguments"]["content"] == "56"
    assert result["route_retry_count"] == 1
    assert result["reason"] == "requirement_retry_pdf_generate"

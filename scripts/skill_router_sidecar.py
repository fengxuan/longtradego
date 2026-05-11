#!/usr/bin/env python3
from __future__ import annotations

import argparse
import ast
import html
import json
import math
import re
import shlex
import time
from datetime import datetime, timezone
from dataclasses import dataclass
from threading import Lock
from typing import Any, Dict, List, Optional, TypedDict

try:
    from langchain_openai import ChatOpenAI
except Exception:
    ChatOpenAI = None


SYMBOL_WITH_MARKET_RE = re.compile(r"\b[A-Z0-9]{1,12}\.(US|HK|SH|SZ|SG|HAS)\b", re.IGNORECASE)
ASCII_TICKER_RE = re.compile(r"(?<![A-Za-z0-9])[A-Z]{1,5}(?![A-Za-z0-9])")
LONGBRIDGE_MENTION_RE = re.compile(r"(?<![A-Za-z0-9_])(longbridge|lb)(?![A-Za-z0-9_])", re.IGNORECASE)
SYMBOL_CANONICAL_RE = re.compile(r"^[A-Z0-9]{1,12}\.(US|HK|SH|SZ|SG|HAS)$", re.IGNORECASE)
QUOTE_KEYWORDS = (
    "quote",
    "price",
    "stock",
    "ticker",
    "market",
    "equity",
    "shares",
    "行情",
    "股价",
    "股票",
    "价格",
    "报价",
    "获取",
    "筛选",
    "screen",
    "macd",
    "pe",
)
WEATHER_KEYWORDS = ("weather", "wttr", "open-meteo", "天气", "气温", "下雨", "温度", "预报")
ARITHMETIC_FRAGMENT_RE = re.compile(r"[0-9\.\+\-\*/%\(\)\sxX×÷]{3,}")
ARITHMETIC_SAFE_EXPR_RE = re.compile(r"^[0-9\.\+\-\*/%\(\)\s]+$")
ARITHMETIC_NUMBER_RE = re.compile(r"-?\d+(?:\.\d+)?")


class RouteState(TypedDict, total=False):
    request: Dict[str, Any]
    cfg: Dict[str, Any]
    candidate_scores: List[Dict[str, Any]]
    entity_hints: Dict[str, Any]
    basic_arithmetic: Dict[str, str]
    selected_skill_id: str
    selected_intent: str
    selected_arguments: Dict[str, Any]
    selected_actions: List[Dict[str, Any]]
    selected_steps: List[Dict[str, Any]]
    recommendations: List[Dict[str, Any]]
    requirement_issues: List[str]
    route_retry_count: int
    confidence: float
    reason: str
    trace: Dict[str, Any]


@dataclass
class RouterConfig:
    auth_token: str
    llm_api_key: str
    llm_base_url: str
    llm_model: str


class SkillRouterService:
    def __init__(self, cfg: RouterConfig):
        self.cfg = cfg
        self.graph, self.planner_backend = self._build_graph()
        self.llm = self._build_llm()
        self.started_at = time.time()
        self.started_at_iso = utc_now_iso()
        self._stats_lock = Lock()
        self.route_requests_total = 0
        self.route_errors_total = 0
        self.last_route_at_iso = ""
        self.last_route_latency_ms = 0
        self.last_skill_id = ""
        self.last_intent = ""
        self.last_reason = ""
        self.last_confidence = 0.0

    def _build_llm(self):
        if ChatOpenAI is None:
            return None
        if not self.cfg.llm_api_key or not self.cfg.llm_base_url:
            return None
        try:
            return ChatOpenAI(
                model=self.cfg.llm_model or "gpt-4.1-mini",
                api_key=self.cfg.llm_api_key,
                base_url=self.cfg.llm_base_url,
                temperature=0,
                timeout=8,
            )
        except Exception:
            return None

    def _build_graph(self):
        try:
            from langgraph.graph import END, StateGraph  # type: ignore

            graph = StateGraph(RouteState)
            graph.add_node("collect_candidates", self.collect_candidates)
            graph.add_node("select_route", self.select_route)
            graph.add_node("enforce_requirements", self.enforce_requirements)
            graph.add_node("build_trace", self.build_trace)
            graph.set_entry_point("collect_candidates")
            graph.add_edge("collect_candidates", "select_route")
            graph.add_edge("select_route", "enforce_requirements")
            graph.add_edge("enforce_requirements", "build_trace")
            graph.add_edge("build_trace", END)
            return graph.compile(), "langgraph"
        except Exception:
            return (
                _SimpleGraph(
                    [self.collect_candidates, self.select_route, self.enforce_requirements, self.build_trace],
                    planner_name="fallback_simple_graph",
                ),
                "fallback_simple_graph",
            )

    def collect_candidates(self, state: RouteState) -> RouteState:
        req = state["request"]
        text = str(req.get("text", "")).strip()
        lower = text.lower()
        installed = req.get("installed_skills", []) or []
        installed_ids = {
            str(item.get("id", "")).strip().lower(): item for item in installed if isinstance(item, dict)
        }

        scores: List[Dict[str, Any]] = []
        for skill_id in installed_ids:
            scores.append({"skill_id": skill_id, "score": 0.0, "signals": []})
        score_map = {item["skill_id"]: item for item in scores}

        def add_signal(skill_id: str, value: float, signal: str) -> None:
            row = score_map.get(skill_id)
            if row is None:
                return
            row["score"] = float(row["score"]) + value
            row["signals"].append(signal)

        def contains_any(keywords: List[str]) -> bool:
            for keyword in keywords:
                token = str(keyword)
                if not token:
                    continue
                if token.isascii():
                    if token.lower() in lower:
                        return True
                elif token in text:
                    return True
            return False

        symbols = extract_symbols(text)
        needs_quote = "longbridge" in score_map and contains_any(list(QUOTE_KEYWORDS))
        needs_weather = "weather" in score_map and contains_any(list(WEATHER_KEYWORDS))
        entity_hints = self._extract_entity_hints(req, needs_quote=needs_quote, needs_weather=needs_weather)
        state["entity_hints"] = entity_hints
        arithmetic = detect_basic_arithmetic_request(text)
        if arithmetic:
            state["basic_arithmetic"] = arithmetic

        if "longbridge" in score_map:
            if LONGBRIDGE_MENTION_RE.search(lower):
                add_signal("longbridge", 0.95, "skill_mentioned")
            if SYMBOL_WITH_MARKET_RE.search(text):
                add_signal("longbridge", 0.55, "symbol_with_market")
            if contains_any(list(QUOTE_KEYWORDS)):
                add_signal("longbridge", 0.7, "market_keywords")
            if symbols:
                add_signal("longbridge", 0.35, "symbol_detected")
            hinted_symbols = entity_hints.get("symbols", [])
            if isinstance(hinted_symbols, list) and hinted_symbols:
                add_signal("longbridge", 0.25, "symbol_inferred")
            hinted_companies = entity_hints.get("company_names", [])
            if isinstance(hinted_companies, list) and hinted_companies:
                add_signal("longbridge", 0.15, "company_inferred")
            if len(score_map["longbridge"]["signals"]) >= 2:
                add_signal("longbridge", 0.1, "combined_market_signals")

        if "weather" in score_map and contains_any(list(WEATHER_KEYWORDS)):
            add_signal("weather", 0.9, "weather_keywords")
            hinted_location = str(entity_hints.get("location", "")).strip()
            if hinted_location:
                add_signal("weather", 0.2, "location_inferred")

        if "pdf" in score_map and contains_any(["pdf", "pdftotext", "qpdf", "extract", "to text", "ocr", "生成", "提取"]):
            add_signal("pdf", 0.9, "pdf_keywords")

        if "find-skills" in score_map and contains_any(
            [
                "skill",
                "skills",
                "支持的skills",
                "支持哪些skill",
                "支持哪些技能",
                "支持的技能",
                "有几个skill",
                "有几个skills",
                "可用技能",
                "可用skill",
                "available skills",
            ]
        ):
            add_signal("find-skills", 0.9, "skills_discovery_keywords")
        if arithmetic and "find-skills" in score_map:
            add_signal("find-skills", 0.35, "arithmetic_fallback")

        for row in scores:
            row["score"] = min(0.99, max(0.0, float(row["score"])))

        scores.sort(key=lambda item: float(item.get("score", 0.0)), reverse=True)
        state["candidate_scores"] = scores
        return state

    def select_route(self, state: RouteState) -> RouteState:
        req = state["request"]
        text = str(req.get("text", "")).strip()
        scores = state.get("candidate_scores", []) or []
        entity_hints = dict(state.get("entity_hints", {}) or {})
        installed = req.get("installed_skills", []) or []
        installed_ids = {str(item.get("id", "")).strip().lower() for item in installed if isinstance(item, dict)}
        arithmetic = state.get("basic_arithmetic", {})

        # Fast path: simple arithmetic should not depend on external LLM latency.
        if isinstance(arithmetic, dict) and arithmetic:
            expression = str(arithmetic.get("expression", "")).strip()
            calc_result = str(arithmetic.get("result", "")).strip()
            pdf_skill_id = pick_pdf_skill_for_generation(installed)
            if expression and calc_result and pdf_skill_id and is_pdf_generate_request_text(text):
                pdf_args = build_pdf_args(text)
                merged_args = {"content": calc_result}
                merged_args.update(pdf_args)
                state["selected_skill_id"] = pdf_skill_id
                state["selected_intent"] = "generate_pdf"
                state["selected_arguments"] = merged_args
                state["selected_actions"] = [{"type": "task", "command": f"task generate pdf with content {shlex.quote(calc_result)}"}]
                state["selected_steps"] = []
                state["confidence"] = 0.98
                state["reason"] = "builtin_arithmetic_to_pdf"
                state["recommendations"] = build_skill_recommendations(
                    text=text,
                    intent=state["selected_intent"],
                    selected_skill_id=state["selected_skill_id"],
                    confidence=float(state.get("confidence", 0.0)),
                    installed_skills=installed,
                    available_skills=req.get("available_skills", []) or [],
                )
                return state

            fallback_skill_id = pick_generic_sys_skill_for_arithmetic(installed)
            if expression and calc_result and fallback_skill_id:
                state["selected_skill_id"] = fallback_skill_id
                state["selected_intent"] = "basic_arithmetic"
                state["selected_arguments"] = {"expression": expression, "result": calc_result}
                state["selected_actions"] = [{"type": "sys", "command": f"echo {shlex.quote(calc_result)}"}]
                state["selected_steps"] = []
                state["confidence"] = 0.96
                state["reason"] = "builtin_arithmetic_fastpath"
                state["recommendations"] = build_skill_recommendations(
                    text=text,
                    intent=state["selected_intent"],
                    selected_skill_id=state["selected_skill_id"],
                    confidence=float(state.get("confidence", 0.0)),
                    installed_skills=installed,
                    available_skills=req.get("available_skills", []) or [],
                )
                return state

        llm_route = self._route_with_llm(req, entity_hints=entity_hints)
        if llm_route:
            state["selected_skill_id"] = str(llm_route.get("skill_id", "")).strip()
            state["selected_intent"] = str(llm_route.get("intent", "")).strip()
            state["selected_arguments"] = dict(llm_route.get("arguments", {}) or {})
            state["selected_actions"] = list(llm_route.get("actions", []) or [])
            state["selected_steps"] = list(llm_route.get("steps", []) or [])
            state["confidence"] = float(llm_route.get("confidence", 0.55))
            state["reason"] = str(llm_route.get("reason", "llm_route")).strip() or "llm_route"
            state["recommendations"] = build_skill_recommendations(
                text=text,
                intent=state["selected_intent"],
                selected_skill_id=state["selected_skill_id"],
                confidence=float(state.get("confidence", 0.0)),
                installed_skills=installed,
                available_skills=req.get("available_skills", []) or [],
            )
            return state

        top = scores[0] if scores else {"skill_id": "", "score": 0.0, "signals": []}
        skill_id = str(top.get("skill_id", "")).strip()
        score = float(top.get("score", 0.0))
        reason = "heuristic"

        if not skill_id and scores:
            skill_id = str(scores[0].get("skill_id", "")).strip()

        if skill_id == "find-skills":
            installed_names = sorted(installed_ids)
            summary = "supported_skills_count={count}; skills={skills}".format(
                count=len(installed_names),
                skills=",".join(installed_names),
            )
            state["selected_skill_id"] = skill_id
            state["selected_intent"] = "list_supported_skills"
            state["selected_arguments"] = {"count": len(installed_names), "skills": installed_names}
            state["selected_actions"] = [{"type": "sys", "command": f"echo {shlex.quote(summary)}"}]
            state["selected_steps"] = []
            state["confidence"] = min(0.99, max(0.01, score))
            state["reason"] = reason if reason else "skills_discovery"
            state["recommendations"] = build_skill_recommendations(
                text=text,
                intent=state["selected_intent"],
                selected_skill_id=state["selected_skill_id"],
                confidence=float(state.get("confidence", 0.0)),
                installed_skills=installed,
                available_skills=req.get("available_skills", []) or [],
            )
            return state

        intent, arguments, actions = plan_route(skill_id, text, entity_hints=entity_hints)
        if not actions:
            actions = [{"type": "sys", "command": "echo 'no action planned'"}]
            score = min(score, 0.3)
            reason = "fallback_action"

        state["selected_skill_id"] = skill_id
        state["selected_intent"] = intent
        state["selected_arguments"] = arguments
        state["selected_actions"] = actions
        state["selected_steps"] = []
        state["confidence"] = min(0.99, max(0.01, score))
        state["reason"] = reason
        state["recommendations"] = build_skill_recommendations(
            text=text,
            intent=intent,
            selected_skill_id=skill_id,
            confidence=float(state.get("confidence", 0.0)),
            installed_skills=installed,
            available_skills=req.get("available_skills", []) or [],
        )
        return state

    def enforce_requirements(self, state: RouteState) -> RouteState:
        req = state["request"]
        text = str(req.get("text", "")).strip()
        installed = req.get("installed_skills", []) or []
        selected_skill_id = str(state.get("selected_skill_id", "")).strip()
        selected_intent = str(state.get("selected_intent", "")).strip()
        selected_arguments = dict(state.get("selected_arguments", {}) or {})
        selected_actions = list(state.get("selected_actions", []) or [])
        selected_steps = list(state.get("selected_steps", []) or [])
        issues: List[str] = []

        if is_pdf_generate_request_text(text):
            if not route_handles_pdf_output(
                selected_skill_id=selected_skill_id,
                selected_intent=selected_intent,
                selected_arguments=selected_arguments,
                selected_actions=selected_actions,
                selected_steps=selected_steps,
            ):
                issues.append("pdf_output_missing")

        if not issues:
            state["requirement_issues"] = []
            state["route_retry_count"] = int(state.get("route_retry_count", 0) or 0)
            return state

        state["requirement_issues"] = issues
        pdf_skill_id = pick_pdf_skill_for_generation(installed)
        if not pdf_skill_id:
            return state

        arithmetic = state.get("basic_arithmetic", {})
        calc_result = ""
        if isinstance(arithmetic, dict):
            calc_result = str(arithmetic.get("result", "")).strip()
        content = calc_result or infer_pdf_content(text)
        pdf_args = build_pdf_args(text)
        merged_args: Dict[str, Any] = {"content": content}
        merged_args.update(pdf_args)
        state["selected_skill_id"] = pdf_skill_id
        state["selected_intent"] = "generate_pdf"
        state["selected_arguments"] = merged_args
        state["selected_actions"] = [{"type": "task", "command": f"task generate pdf with content {shlex.quote(content)}"}]
        state["selected_steps"] = []
        state["confidence"] = max(float(state.get("confidence", 0.0)), 0.95)
        state["reason"] = "requirement_retry_pdf_generate"
        state["route_retry_count"] = int(state.get("route_retry_count", 0) or 0) + 1
        state["recommendations"] = build_skill_recommendations(
            text=text,
            intent=state["selected_intent"],
            selected_skill_id=state["selected_skill_id"],
            confidence=float(state.get("confidence", 0.0)),
            installed_skills=installed,
            available_skills=req.get("available_skills", []) or [],
        )
        return state

    def build_trace(self, state: RouteState) -> RouteState:
        state["trace"] = {
            "candidate_scores": state.get("candidate_scores", []),
            "entity_hints": state.get("entity_hints", {}),
            "planner": self.planner_backend,
            "reason": state.get("reason", ""),
            "route_steps_count": len(state.get("selected_steps", []) or []),
            "recommendations_count": len(state.get("recommendations", []) or []),
            "route_retry_count": int(state.get("route_retry_count", 0) or 0),
            "requirement_issues": state.get("requirement_issues", []) or [],
        }
        return state

    def _route_with_llm(self, req: Dict[str, Any], entity_hints: Optional[Dict[str, Any]] = None) -> Dict[str, Any]:
        if self.llm is None:
            return {}
        text = str(req.get("text", "")).strip()
        installed = req.get("installed_skills", []) or []
        installed_rows = [item for item in installed if isinstance(item, dict)]
        installed_ids = {str(item.get("id", "")).strip() for item in installed_rows if str(item.get("id", "")).strip()}
        if not text or not installed_ids:
            return {}

        prompt_payload = {
            "text": text,
            "installed_skills": [
                {
                    "id": str(item.get("id", "")).strip(),
                    "description": str(item.get("description", "")).strip(),
                    "executor": str(item.get("executor", "")).strip(),
                    "command_hints": item.get("command_hints", []) if isinstance(item.get("command_hints", []), list) else [],
                    "allowed_actions": item.get("allowed_actions", []) if isinstance(item.get("allowed_actions", []), list) else [],
                }
                for item in installed_rows
            ],
            "entity_hints": dict(entity_hints or {}),
        }
        prompt = (
            "You are an intent router for local skills automation.\n"
            "Return strict JSON only (no markdown, no prose).\n"
            "Schema:\n"
            '{"skill_id":string,"intent":string,"arguments":object,"confidence":number(0..1),'
            '"actions":[{"type":"lb|email|task|sys","command":string}],'
            '"steps":[{"skill_id":string,"intent":string,"arguments":object,'
            '"actions":[{"type":"lb|email|task|sys","command":string}],"reason":string}],"reason":string}\n'
            "Rules:\n"
            "1) skill_id and every steps[].skill_id must be from installed_skills ids.\n"
            "2) If one skill can solve the request, return it directly with steps as [].\n"
            "3) If multiple skills are needed, return steps with all required skills in execution order.\n"
            "4) For multi-skill workflows, set skill_id to \"multi-skill\" and intent to \"multi_skill_workflow\".\n"
            "5) Always include at least one runnable action either in actions or in steps[].actions.\n"
            "6) Never invent unavailable commands or skill ids.\n"
            "7) Preserve user-requested ordering constraints when they are explicit and feasible.\n"
            "Input JSON:\n"
            f"{json.dumps(prompt_payload, ensure_ascii=False)}"
        )
        try:
            response = self.llm.invoke(prompt)
            content = getattr(response, "content", "")
            if isinstance(content, list):
                content = " ".join(str(part) for part in content)
            parsed = extract_json_object(str(content))
            if not isinstance(parsed, dict):
                return {}

            def normalize_actions(raw: Any) -> List[Dict[str, Any]]:
                out: List[Dict[str, Any]] = []
                if not isinstance(raw, list):
                    return out
                for one in raw:
                    if not isinstance(one, dict):
                        continue
                    action_type = str(one.get("type", "")).strip().lower()
                    command = str(one.get("command", "")).strip()
                    if not action_type and not command:
                        continue
                    out.append({"type": action_type, "command": command})
                return out

            def normalize_step(raw: Any) -> Optional[Dict[str, Any]]:
                if not isinstance(raw, dict):
                    return None
                step_skill_id = str(raw.get("skill_id", "")).strip()
                if step_skill_id not in installed_ids:
                    return None
                step_arguments = raw.get("arguments", {})
                if not isinstance(step_arguments, dict):
                    step_arguments = {}
                return {
                    "skill_id": step_skill_id,
                    "intent": str(raw.get("intent", "")).strip(),
                    "arguments": dict(step_arguments),
                    "actions": normalize_actions(raw.get("actions", [])),
                    "reason": str(raw.get("reason", "")).strip(),
                }

            normalized_steps: List[Dict[str, Any]] = []
            for raw_step in parsed.get("steps", []) if isinstance(parsed.get("steps", []), list) else []:
                step = normalize_step(raw_step)
                if step is not None:
                    normalized_steps.append(step)

            skill_id = str(parsed.get("skill_id", "")).strip()
            if len(normalized_steps) > 1:
                skill_id = "multi-skill"
            elif len(normalized_steps) == 1:
                if skill_id == "" or skill_id == "multi-skill":
                    skill_id = str(normalized_steps[0].get("skill_id", "")).strip()

            if not normalized_steps and skill_id not in installed_ids:
                return {}

            arguments = parsed.get("arguments", {})
            if not isinstance(arguments, dict):
                arguments = {}

            actions = normalize_actions(parsed.get("actions", []))
            if not actions and normalized_steps:
                for step in normalized_steps:
                    for action in step.get("actions", []) or []:
                        if isinstance(action, dict):
                            actions.append(action)
            if not actions:
                return {}

            intent = str(parsed.get("intent", "")).strip()
            if len(normalized_steps) > 1 and not intent:
                intent = "multi_skill_workflow"
            if len(normalized_steps) == 1 and not intent:
                intent = str(normalized_steps[0].get("intent", "")).strip()

            confidence = 0.55
            try:
                confidence = float(parsed.get("confidence", 0.55))
            except Exception:
                confidence = 0.55
            confidence = max(0.01, min(0.99, confidence))

            reason = str(parsed.get("reason", "")).strip() or "llm_route"
            return {
                "skill_id": skill_id,
                "intent": intent,
                "arguments": dict(arguments),
                "actions": actions,
                "steps": normalized_steps,
                "confidence": confidence,
                "reason": reason,
            }
        except Exception:
            return {}
        return {}

    def _extract_entity_hints(self, req: Dict[str, Any], needs_quote: bool, needs_weather: bool) -> Dict[str, Any]:
        text = str(req.get("text", "")).strip()
        hints: Dict[str, Any] = {}
        if not text:
            return hints

        runtime_context = req.get("runtime_context", {})
        if isinstance(runtime_context, dict):
            from_runtime = runtime_context.get("entity_hints", {})
            if isinstance(from_runtime, dict):
                hints.update(dict(from_runtime))

        local_symbols = extract_symbols(text)
        if local_symbols:
            hints["symbols"] = local_symbols

        local_companies = infer_company_names_from_text(text)
        if local_companies:
            hints["company_names"] = local_companies

        local_location = infer_weather_location_from_text(text)
        if local_location:
            hints["location"] = local_location

        wants_llm_enrichment = (
            self.llm is not None
            and ((needs_quote and not hints.get("symbols")) or (needs_weather and not hints.get("location")))
        )
        if wants_llm_enrichment:
            installed = req.get("installed_skills", []) or []
            installed_ids = [str(item.get("id", "")).strip() for item in installed if isinstance(item, dict)]
            prompt = (
                "Extract routing entities from the user request and return strict JSON only.\n"
                "Output schema:\n"
                '{"symbols":[string], "location":string, "company_names":[string], "confidence":number}\n'
                "Rules:\n"
                "1) symbols must be uppercase and include market suffix like TSLA.US or 700.HK.\n"
                "2) If the user gives company names but not symbols, infer likely listed symbol(s).\n"
                "3) If weather location is absent, return empty string.\n"
                "4) No extra keys, no markdown.\n"
                f"Installed skills: {installed_ids}\n"
                f"User request: {text}"
            )
            try:
                response = self.llm.invoke(prompt)
                content = getattr(response, "content", "")
                if isinstance(content, list):
                    content = " ".join(str(part) for part in content)
                parsed = extract_json_object(str(content))
                if isinstance(parsed, dict):
                    merged_symbols = merge_symbol_hints(local_symbols, parsed.get("symbols"))
                    if merged_symbols:
                        hints["symbols"] = merged_symbols
                    merged_companies = merge_text_hints(local_companies, parsed.get("company_names"))
                    if merged_companies:
                        hints["company_names"] = merged_companies
                    location = normalize_location(str(parsed.get("location", "")).strip())
                    if location:
                        hints["location"] = location
            except Exception:
                pass

        merged_symbols = merge_symbol_hints(local_symbols, hints.get("symbols"))
        if merged_symbols:
            hints["symbols"] = merged_symbols
        else:
            hints.pop("symbols", None)
        merged_companies = merge_text_hints(local_companies, hints.get("company_names"))
        if merged_companies:
            hints["company_names"] = merged_companies
        else:
            hints.pop("company_names", None)
        normalized_location = normalize_location(str(hints.get("location", "")).strip())
        if normalized_location:
            hints["location"] = normalized_location
        else:
            hints.pop("location", None)
        return hints

    def route(self, request_payload: Dict[str, Any]) -> Dict[str, Any]:
        started = time.perf_counter()
        selected_skill_id = ""
        selected_intent = ""
        selected_reason = ""
        selected_confidence = 0.0
        errors_total_delta = 0
        try:
            state: RouteState = {"request": request_payload, "cfg": {}}
            final_state = self.graph.invoke(state)
            selected_skill_id = str(final_state.get("selected_skill_id", "")).strip()
            selected_intent = str(final_state.get("selected_intent", "")).strip()
            selected_reason = str(final_state.get("reason", "")).strip()
            selected_confidence = float(final_state.get("confidence", 0.0))
            latency_ms = int((time.perf_counter() - started) * 1000)
            trace = dict(final_state.get("trace", {}) or {})
            trace["latency_ms"] = latency_ms
            return {
                "skill_id": selected_skill_id,
                "intent": selected_intent,
                "arguments": final_state.get("selected_arguments", {}) or {},
                "confidence": selected_confidence,
                "actions": final_state.get("selected_actions", []) or [],
                "steps": final_state.get("selected_steps", []) or [],
                "recommendations": final_state.get("recommendations", []) or [],
                "reason": selected_reason,
                "trace": trace,
                "errors": [],
            }
        except Exception:
            errors_total_delta = 1
            raise
        finally:
            latency_ms = int((time.perf_counter() - started) * 1000)
            with self._stats_lock:
                self.route_requests_total += 1
                self.route_errors_total += errors_total_delta
                self.last_route_at_iso = utc_now_iso()
                self.last_route_latency_ms = latency_ms
                self.last_skill_id = selected_skill_id
                self.last_intent = selected_intent
                self.last_reason = selected_reason
                self.last_confidence = selected_confidence

    def status_payload(self) -> Dict[str, Any]:
        with self._stats_lock:
            requests_total = self.route_requests_total
            errors_total = self.route_errors_total
            last_route_at_iso = self.last_route_at_iso
            last_route_latency_ms = self.last_route_latency_ms
            last_skill_id = self.last_skill_id
            last_intent = self.last_intent
            last_reason = self.last_reason
            last_confidence = self.last_confidence

        success_total = max(0, requests_total - errors_total)
        success_rate = 1.0 if requests_total == 0 else float(success_total) / float(requests_total)
        uptime_seconds = max(0, int(time.time() - self.started_at))
        return {
            "status": "ok",
            "service": {
                "name": "Longtrade Skill Router",
                "version": "2.1.0",
                "planner": self.planner_backend,
                "langchain_available": ChatOpenAI is not None,
                "llm_enabled": self.llm is not None,
            },
            "started_at": self.started_at_iso,
            "uptime_seconds": uptime_seconds,
            "router": {
                "requests_total": requests_total,
                "success_total": success_total,
                "errors_total": errors_total,
                "success_rate": round(success_rate, 6),
                "last_route_at": last_route_at_iso,
                "last_route_latency_ms": last_route_latency_ms,
                "last_skill_id": last_skill_id,
                "last_intent": last_intent,
                "last_reason": last_reason,
                "last_confidence": last_confidence,
            },
            "endpoints": {
                "route": "/v1/route",
                "status_json": "/v1/status",
                "status_page": "/status",
                "healthz": "/healthz",
            },
        }


def extract_json_object(raw: str) -> Dict[str, Any]:
    content = str(raw or "").strip()
    if not content:
        return {}
    try:
        parsed = json.loads(content)
        if isinstance(parsed, dict):
            return parsed
    except Exception:
        pass
    fenced = re.search(r"```(?:json)?\s*(\{.*?\})\s*```", content, flags=re.IGNORECASE | re.DOTALL)
    if fenced:
        try:
            parsed = json.loads(fenced.group(1))
            if isinstance(parsed, dict):
                return parsed
        except Exception:
            pass
    match = re.search(r"\{.*\}", content, flags=re.DOTALL)
    if not match:
        return {}
    try:
        parsed = json.loads(match.group(0))
        if isinstance(parsed, dict):
            return parsed
    except Exception:
        return {}
    return {}


def normalize_symbol_candidate(raw: str) -> str:
    symbol = str(raw or "").strip().upper()
    if not symbol:
        return ""
    if SYMBOL_CANONICAL_RE.match(symbol):
        return symbol
    return ""


def merge_symbol_hints(primary: Any, secondary: Any = None) -> List[str]:
    merged: List[str] = []
    seen: set[str] = set()

    def add_many(value: Any) -> None:
        if isinstance(value, str):
            normalized = normalize_symbol_candidate(value)
            if normalized and normalized not in seen:
                seen.add(normalized)
                merged.append(normalized)
            return
        if not isinstance(value, list):
            return
        for one in value:
            normalized = normalize_symbol_candidate(str(one))
            if normalized and normalized not in seen:
                seen.add(normalized)
                merged.append(normalized)

    add_many(primary)
    add_many(secondary)
    return merged


def normalize_company_name(raw: str) -> str:
    value = str(raw or "").strip()
    if not value:
        return ""
    value = value.replace("\n", " ").replace("\r", " ")
    value = value.strip(" ,，。！？；;:：")
    value = re.sub(r"^(当前|现在|今日|今天|请问|请|帮我|查询|查看|获取|告诉我|想看)\s*", "", value)
    value = value.strip(" ,，。！？；;:：")
    return value


def merge_text_hints(primary: Any, secondary: Any = None) -> List[str]:
    merged: List[str] = []
    seen: set[str] = set()

    def add_many(value: Any) -> None:
        if isinstance(value, str):
            normalized = normalize_company_name(value)
            if normalized and normalized not in seen:
                seen.add(normalized)
                merged.append(normalized)
            return
        if not isinstance(value, list):
            return
        for one in value:
            normalized = normalize_company_name(str(one))
            if normalized and normalized not in seen:
                seen.add(normalized)
                merged.append(normalized)

    add_many(primary)
    add_many(secondary)
    return merged


def extract_symbols(text: str) -> List[str]:
    upper = text.upper()
    symbols = {m.group(0).upper() for m in SYMBOL_WITH_MARKET_RE.finditer(upper)}
    ignore = {
        "US", "HK", "SH", "SZ", "SG", "HAS", "PE", "MACD", "RSI", "USD", "CNY", "HKD"
    }
    for match in ASCII_TICKER_RE.finditer(text):
        token_raw = match.group(0)
        if token_raw != token_raw.upper():
            continue
        token = token_raw.upper()
        if token in ignore:
            continue
        symbols.add(token + ".US")
    return sorted(symbols)


def normalize_arithmetic_expression(raw: str) -> str:
    candidate = str(raw or "").strip()
    if not candidate:
        return ""
    candidate = (
        candidate.replace("（", "(")
        .replace("）", ")")
        .replace("＋", "+")
        .replace("－", "-")
        .replace("×", "*")
        .replace("÷", "/")
    )
    candidate = re.sub(r"(?<=\d)\s*[xX]\s*(?=\d)", "*", candidate)
    candidate = candidate.strip(" =＝,，。.!！?？;；:：")
    candidate = re.sub(r"\s+", " ", candidate).strip()
    return candidate


def safe_eval_arithmetic_expression(expression: str) -> Optional[float]:
    trimmed = str(expression or "").strip()
    if not trimmed:
        return None
    if not ARITHMETIC_SAFE_EXPR_RE.fullmatch(trimmed):
        return None
    try:
        tree = ast.parse(trimmed, mode="eval")
    except Exception:
        return None

    def evaluate(node: ast.AST) -> float:
        if isinstance(node, ast.Expression):
            return evaluate(node.body)
        if isinstance(node, ast.Constant):
            if isinstance(node.value, (int, float)):
                return float(node.value)
            raise ValueError("unsupported constant")
        if isinstance(node, ast.Num):
            return float(node.n)
        if isinstance(node, ast.UnaryOp):
            value = evaluate(node.operand)
            if isinstance(node.op, ast.UAdd):
                return value
            if isinstance(node.op, ast.USub):
                return -value
            raise ValueError("unsupported unary op")
        if isinstance(node, ast.BinOp):
            left = evaluate(node.left)
            right = evaluate(node.right)
            if isinstance(node.op, ast.Add):
                return left + right
            if isinstance(node.op, ast.Sub):
                return left - right
            if isinstance(node.op, ast.Mult):
                return left * right
            if isinstance(node.op, ast.Div):
                if right == 0:
                    raise ValueError("division by zero")
                return left / right
            if isinstance(node.op, ast.FloorDiv):
                if right == 0:
                    raise ValueError("division by zero")
                return left // right
            if isinstance(node.op, ast.Mod):
                if right == 0:
                    raise ValueError("division by zero")
                return left % right
            if isinstance(node.op, ast.Pow):
                if abs(right) > 12 or abs(left) > 1e6:
                    raise ValueError("power overflow risk")
                return left ** right
            raise ValueError("unsupported binary op")
        raise ValueError("unsupported node")

    try:
        value = float(evaluate(tree))
    except Exception:
        return None
    if not math.isfinite(value):
        return None
    if abs(value) > 1e12:
        return None
    return value


def format_arithmetic_result(value: float) -> str:
    rounded = round(value)
    if abs(value - rounded) <= 1e-9:
        return str(int(rounded))
    return f"{value:.10f}".rstrip("0").rstrip(".")


def detect_basic_arithmetic_request(text: str) -> Optional[Dict[str, str]]:
    raw_text = str(text or "").strip()
    if not raw_text:
        return None
    if len(ARITHMETIC_NUMBER_RE.findall(raw_text)) < 2:
        return None
    normalized = normalize_arithmetic_expression(raw_text)
    fragments = [normalize_arithmetic_expression(one) for one in ARITHMETIC_FRAGMENT_RE.findall(normalized)]
    if normalized and normalized not in fragments:
        fragments.append(normalized)

    best: Optional[Dict[str, str]] = None
    for fragment in sorted(fragments, key=lambda one: len(one), reverse=True):
        if not fragment:
            continue
        if len(ARITHMETIC_NUMBER_RE.findall(fragment)) < 2:
            continue
        if not any(op in fragment for op in ["+", "-", "*", "/", "%"]):
            continue
        value = safe_eval_arithmetic_expression(fragment)
        if value is None:
            continue
        candidate = {"expression": fragment, "result": format_arithmetic_result(value)}
        if best is None:
            best = candidate
            continue
        if len(candidate["expression"]) > len(best["expression"]):
            best = candidate
    return best


def pick_generic_sys_skill_for_arithmetic(installed_skills: Any) -> str:
    rows = [item for item in installed_skills if isinstance(item, dict)]
    by_id = {str(item.get("id", "")).strip().lower(): item for item in rows if str(item.get("id", "")).strip()}

    def supports_generic_sys(item: Dict[str, Any]) -> bool:
        executor = str(item.get("executor", "")).strip().lower()
        if executor not in ("", "actions_v1"):
            return False
        allowed = item.get("allowed_actions", [])
        if not isinstance(allowed, list) or len(allowed) == 0:
            return True
        allowed_set = {str(one).strip().lower() for one in allowed if str(one).strip()}
        return "sys" in allowed_set or "task" in allowed_set

    def looks_like_calculator(item: Dict[str, Any]) -> bool:
        skill_id = str(item.get("id", "")).strip().lower()
        description = str(item.get("description", "")).strip().lower()
        text = skill_id + " " + description
        return any(token in text for token in ["calculator", "calculation", "calc", "math", "算术", "计算"])

    for row in rows:
        if looks_like_calculator(row) and supports_generic_sys(row):
            return str(row.get("id", "")).strip().lower()

    for preferred in ["find-skills", "weather", "pdf"]:
        row = by_id.get(preferred)
        if isinstance(row, dict) and supports_generic_sys(row):
            return preferred
    for row in rows:
        skill_id = str(row.get("id", "")).strip().lower()
        if not skill_id:
            continue
        if supports_generic_sys(row):
            return skill_id
    return ""


def normalize_location(value: str) -> str:
    text = str(value or "").strip()
    if not text:
        return ""
    text = text.replace("\n", " ").replace("\r", " ").strip()
    text = text.strip(" ,，。！？；;:：")
    text = re.sub(r"^(当前|现在|今日|今天|请问|请|帮我|查询|查看|获取|告诉我|想看)\s*", "", text)
    text = re.sub(r"\s*(的)?(天气|气温|温度|预报)$", "", text)
    text = re.sub(r"\s+(today|now|tomorrow|tonight|this\s+week)\s*$", "", text, flags=re.IGNORECASE)
    text = text.strip(" ,，。！？；;:：")
    return text


def infer_weather_location_from_text(text: str) -> str:
    candidates: List[str] = []
    patterns = [
        r"weather(?:\s+in|\s+for)?\s+([A-Za-z][A-Za-z\s\-]{1,40})",
        r"(?:^|[\s,，。！？])([\u4e00-\u9fffA-Za-z]{1,24}?)(?:的)?天气",
        r"(?:天气|气温|温度|预报)(?:在|in)?\s*([\u4e00-\u9fffA-Za-z][\u4e00-\u9fffA-Za-z\s\-]{0,30})",
    ]
    for pattern in patterns:
        for match in re.finditer(pattern, text, flags=re.IGNORECASE):
            candidate = normalize_location(match.group(1))
            if candidate:
                candidates.append(candidate)
    if not candidates:
        return ""
    return candidates[0]


def infer_company_names_from_text(text: str) -> List[str]:
    candidates: List[str] = []
    patterns = [
        r"(?:^|[\s,，。！？])([\u4e00-\u9fffA-Za-z0-9&\.\-]{1,24}?)(?:股票|股价|报价|价格)",
        r"(?:stock|price)\s+(?:of\s+)?([A-Za-z][A-Za-z0-9&\.\-\s]{1,30})",
    ]
    for pattern in patterns:
        for match in re.finditer(pattern, text, flags=re.IGNORECASE):
            candidate = normalize_company_name(match.group(1))
            if candidate:
                candidates.append(candidate)
    return merge_text_hints(candidates)


def build_pdf_args(text: str) -> Dict[str, Any]:
    lower = text.lower()
    args: Dict[str, Any] = {}
    pdf_matches = re.findall(r"([\w\-/\.]+\.pdf)", text)
    if pdf_matches:
        args["input_file"] = pdf_matches[0]
        if len(pdf_matches) > 1:
            args["output_file"] = pdf_matches[-1]
    if "layout" in lower:
        args["layout"] = True
    return args


def is_pdf_generate_request_text(text: str) -> bool:
    raw = str(text or "").strip()
    if not raw:
        return False
    lower = raw.lower()
    if "pdf" not in lower and ".pdf" not in lower:
        return False
    keywords = [
        "generate", "create", "make", "build", "write", "save", "export", "render", "output",
        "to pdf", "as pdf", "pdf file",
        "生成", "创建", "制作", "保存", "导出", "输出", "写入", "存成", "另存为", "转成pdf", "输出到pdf", "保存到pdf",
    ]
    return any(keyword in lower for keyword in keywords)


def pick_pdf_skill_for_generation(installed_skills: Any) -> str:
    rows = [item for item in installed_skills if isinstance(item, dict)]

    def supports_pdf_actions(item: Dict[str, Any]) -> bool:
        allowed = item.get("allowed_actions", [])
        if not isinstance(allowed, list) or len(allowed) == 0:
            return True
        allowed_set = {str(one).strip().lower() for one in allowed if str(one).strip()}
        return "sys" in allowed_set or "task" in allowed_set

    for row in rows:
        skill_id = str(row.get("id", "")).strip()
        if skill_id.lower() == "pdf" and supports_pdf_actions(row):
            return skill_id
    for row in rows:
        skill_id = str(row.get("id", "")).strip()
        description = str(row.get("description", "")).strip().lower()
        if not skill_id:
            continue
        if "pdf" in skill_id.lower() or "pdf" in description:
            if supports_pdf_actions(row):
                return skill_id
    return ""


def route_handles_pdf_output(
    selected_skill_id: str,
    selected_intent: str,
    selected_arguments: Any,
    selected_actions: Any,
    selected_steps: Any,
) -> bool:
    if str(selected_skill_id or "").strip().lower() == "pdf":
        return True
    if "pdf" in str(selected_intent or "").strip().lower():
        return True

    args = selected_arguments if isinstance(selected_arguments, dict) else {}
    for key in ["output", "output_file", "file", "path", "filename", "input_file"]:
        value = str(args.get(key, "")).strip().lower()
        if value.endswith(".pdf"):
            return True

    rows = selected_steps if isinstance(selected_steps, list) else []
    for one in rows:
        if not isinstance(one, dict):
            continue
        step_skill = str(one.get("skill_id", "")).strip().lower()
        step_intent = str(one.get("intent", "")).strip().lower()
        if step_skill == "pdf" or "pdf" in step_intent:
            return True

    actions = selected_actions if isinstance(selected_actions, list) else []
    for one in actions:
        if not isinstance(one, dict):
            continue
        command = str(one.get("command", "")).strip().lower()
        if ".pdf" in command or "pdf" in command:
            return True
    return False


def normalize_pdf_content(value: str) -> str:
    candidate = str(value or "").strip().strip("'\"")
    if not candidate:
        return ""
    candidate = re.sub(r"(?i)\s*(?:到|至|入|进|保存到|输出到|写入到|写到|存到)\s*pdf(?:文件|文档|檔案|file)?\s*$", "", candidate)
    candidate = re.sub(r"(?i)^(?:请|请帮我|帮我|麻烦|可以|可否|能否|把|将)\s*", "", candidate)
    candidate = re.sub(r"(?i)^(?:写入|写上|写成|写下|保存|输出|存入)\s*", "", candidate)
    candidate = candidate.strip(" ,，。！？；;:：'\"")
    return candidate


def infer_pdf_content(text: str) -> str:
    raw = str(text or "").strip()
    if not raw:
        return "hello world"

    quoted = re.search(r"'([^']+)'|\"([^\"]+)\"", raw)
    if quoted:
        candidate = normalize_pdf_content(quoted.group(1) or quoted.group(2))
        if candidate:
            return candidate

    patterns = [
        r"(?i)(?:写入|写上|写成|写下|保存|输出|存入)\s*(.+)\s*(?:到|至|入|进)\s*pdf(?:文件|文档|檔案|file)?",
        r"(?i)(?:把|将)\s*(.+)\s*(?:写入|写到|保存到|输出到|存到)\s*pdf(?:文件|文档|檔案|file)?",
        r"(?i)(?:里面写|内容(?:是|为)?|文字(?:是|为)?|文本(?:是|为)?)\s*[:：=]?\s*(.+)$",
        r"(?i)(?:write|save|put)\s+(.+?)\s+(?:to|into)\s+(?:a\s+)?pdf(?:\s+file)?",
    ]
    for pattern in patterns:
        match = re.search(pattern, raw, flags=re.IGNORECASE)
        if not match:
            continue
        candidate = normalize_pdf_content(match.group(1))
        if candidate:
            return candidate

    lower = raw.lower()
    pdf_idx = lower.find("pdf")
    if pdf_idx > 0:
        prefix = normalize_pdf_content(raw[:pdf_idx])
        if prefix:
            return prefix

    if "hello world" in lower:
        return "hello world"
    return "hello world"


def normalize_capabilities(raw: Any) -> List[str]:
    values: List[str] = []
    if isinstance(raw, list):
        values = [str(item or "").strip() for item in raw]
    elif isinstance(raw, str):
        values = [item.strip() for item in raw.split(",")]
    out: List[str] = []
    seen = set()
    for value in values:
        capability = value.lower().strip()
        if not capability or capability in seen:
            continue
        seen.add(capability)
        out.append(capability)
    return sorted(out)


def build_install_command(source: str, skill_id: str, install_template: str) -> str:
    template = str(install_template or "").strip()
    if template:
        return (
            template.replace("{source}", str(source or "").strip()).replace("{skill_id}", str(skill_id or "").strip()).strip()
        )
    install_source = str(source or "").strip() or str(skill_id or "").strip()
    if not install_source:
        return ""
    return f"npx skills add {shlex.quote(install_source)} -g -y"


def normalize_available_skills(raw: Any) -> List[Dict[str, Any]]:
    if not isinstance(raw, list):
        return []
    out: List[Dict[str, Any]] = []
    seen = set()
    for item in raw:
        if not isinstance(item, dict):
            continue
        skill_id = str(item.get("id", "")).strip()
        if not skill_id:
            continue
        skill_key = skill_id.lower()
        if skill_key in seen:
            continue
        seen.add(skill_key)

        priority = 0
        raw_priority = item.get("priority", 0)
        try:
            priority = int(raw_priority)
        except Exception:
            priority = 0

        out.append(
            {
                "id": skill_id,
                "capabilities": normalize_capabilities(item.get("capabilities", [])),
                "priority": priority,
                "source": str(item.get("source", "")).strip(),
                "source_url": str(item.get("source_url", "")).strip(),
                "install_command_template": str(item.get("install_command_template", "")).strip(),
            }
        )
    out.sort(key=lambda row: (-int(row.get("priority", 0)), str(row.get("id", "")).lower()))
    return out


def infer_skill_capabilities(skill_id: str, description: str, catalog_caps: Any = None) -> List[str]:
    inferred = set(normalize_capabilities(catalog_caps))
    key = str(skill_id or "").strip().lower()
    text = str(description or "").strip().lower()

    if key == "pdf" or "pdf" in text:
        inferred.add("pdf")
    if key == "weather" or any(token in text for token in ["weather", "wttr", "open-meteo", "天气", "预报"]):
        inferred.add("weather")
    if key == "longbridge" or any(token in text for token in ["longbridge", "stock", "quote", "行情", "股票"]):
        inferred.add("market_data")
    if key == "find-skills" or any(token in text for token in ["skill", "skills", "技能"]):
        inferred.add("skill_discovery")
    if "calc" in key or "math" in key or any(token in text for token in ["calculator", "calculation", "math", "算术", "计算"]):
        inferred.add("calculation")
    return sorted(inferred)


def infer_request_capabilities(text: str, intent: str) -> List[str]:
    request_text = str(text or "")
    lower_text = request_text.lower()
    lower_intent = str(intent or "").lower()
    capabilities = set()

    if any(token in lower_text for token in ["pdf", "pdftotext", "qpdf", "ocr", "extract"]) or "pdf" in lower_intent or "提取" in request_text or "生成" in request_text:
        capabilities.add("pdf")
    if any(token in lower_text for token in WEATHER_KEYWORDS) or "weather" in lower_intent or "天气" in request_text:
        capabilities.add("weather")
    if any(token in lower_text for token in QUOTE_KEYWORDS) or "quote" in lower_intent or "stock" in lower_intent:
        capabilities.add("market_data")
    if any(token in lower_text for token in ["skill", "skills"]) or "技能" in request_text or "find-skills" in lower_intent:
        capabilities.add("skill_discovery")
    if detect_basic_arithmetic_request(request_text) is not None or any(
        token in lower_text for token in ["calculate", "calculator", "math", "compute", "1+1"]
    ) or any(token in request_text for token in ["计算", "算一下", "等于多少", "求值"]):
        capabilities.add("calculation")
    return sorted(capabilities)


def build_skill_recommendations(
    text: str,
    intent: str,
    selected_skill_id: str,
    confidence: float,
    installed_skills: Any,
    available_skills: Any,
) -> List[Dict[str, Any]]:
    installed_rows = [item for item in installed_skills if isinstance(item, dict)]
    installed_ids = {str(item.get("id", "")).strip().lower() for item in installed_rows if str(item.get("id", "")).strip()}
    installed_map = {str(item.get("id", "")).strip().lower(): item for item in installed_rows if isinstance(item, dict)}
    normalized_available = normalize_available_skills(available_skills)
    if not normalized_available:
        return []

    available_map = {str(item.get("id", "")).strip().lower(): item for item in normalized_available}
    selected_key = str(selected_skill_id or "").strip().lower()
    selected_row = installed_map.get(selected_key, {})
    selected_catalog = available_map.get(selected_key, {})
    selected_caps = set(
        infer_skill_capabilities(
            selected_skill_id,
            str(selected_row.get("description", "")).strip(),
            selected_catalog.get("capabilities", []),
        )
    )
    request_caps = set(infer_request_capabilities(text, intent))
    selected_priority: Optional[int] = None
    if selected_catalog:
        try:
            selected_priority = int(selected_catalog.get("priority", 0))
        except Exception:
            selected_priority = 0

    recommendations: List[Dict[str, Any]] = []
    seen = set()

    def append_recommendation(item: Dict[str, Any], reason: str, overlap_caps: List[str]) -> None:
        skill_id = str(item.get("id", "")).strip()
        if not skill_id:
            return
        key = f"{skill_id.lower()}::{reason}"
        if key in seen:
            return
        seen.add(key)
        source = str(item.get("source", "")).strip() or str(item.get("source_url", "")).strip() or skill_id
        install_command = build_install_command(source, skill_id, str(item.get("install_command_template", "")).strip())
        caps = overlap_caps if overlap_caps else normalize_capabilities(item.get("capabilities", []))
        recommendations.append(
            {
                "skill_id": skill_id,
                "reason": reason,
                "capabilities": caps,
                "install_source": source,
                "install_command": install_command,
            }
        )

    if request_caps:
        capability_mismatch = not selected_caps or request_caps.isdisjoint(selected_caps)
        if confidence < 0.55 or capability_mismatch:
            for item in normalized_available:
                if str(item.get("id", "")).strip().lower() in installed_ids:
                    continue
                overlap = sorted(request_caps.intersection(set(normalize_capabilities(item.get("capabilities", [])))))
                if not overlap:
                    continue
                append_recommendation(item, "missing_capability", overlap)
                if len(recommendations) >= 3:
                    break

    if selected_priority is not None and selected_caps:
        for item in normalized_available:
            candidate_key = str(item.get("id", "")).strip().lower()
            if candidate_key in installed_ids:
                continue
            try:
                candidate_priority = int(item.get("priority", 0))
            except Exception:
                candidate_priority = 0
            if candidate_priority <= selected_priority:
                continue
            overlap = sorted(selected_caps.intersection(set(normalize_capabilities(item.get("capabilities", [])))))
            if not overlap:
                continue
            append_recommendation(item, "better_match_available", overlap)
            if len(recommendations) >= 5:
                break

    return recommendations


def plan_route(skill_id: str, text: str, entity_hints: Optional[Dict[str, Any]] = None):
    lower = text.lower()
    symbols = infer_quote_symbols(text, entity_hints=entity_hints)
    quote_targets = infer_quote_targets(text, entity_hints=entity_hints)

    if skill_id == "longbridge":
        if any(k in lower for k in ["screen", "筛选", "macd", "市值", "pe"]):
            return (
                "market_screen_v1",
                {"symbols": symbols, "market_cap_usd_min": 50000000000, "pe_max": 25, "macd_lookback_days": 5},
                [{"type": "lb", "command": "calc-index"}],
            )
        if symbols:
            return (
                "get_stock_price",
                {"symbols": symbols},
                [{"type": "lb", "command": "quote " + " ".join(symbols)}],
            )
        if quote_targets:
            target_args = [shlex.quote(item) for item in quote_targets]
            return (
                "get_stock_price",
                {"targets": quote_targets},
                [{"type": "lb", "command": "quote " + " ".join(target_args) + " --format json"}],
            )
        return (
            "get_stock_price",
            {"query": text},
            [{"type": "sys", "command": "echo 'unable to resolve stock symbols; please provide ticker like TSLA.US or 700.HK'"}],
        )

    if skill_id == "weather":
        city = infer_weather_location(text, entity_hints=entity_hints)
        location_path = city.replace(" ", "+") if city else ""
        weather_url = f"wttr.in/{location_path}?format=3" if location_path else "wttr.in/?format=3"
        weather_args: Dict[str, Any] = {}
        if city:
            weather_args["location"] = city
        return (
            "get_weather",
            weather_args,
            [{"type": "sys", "command": f'curl -s "{weather_url}"'}],
        )

    if skill_id == "pdf":
        pdf_args = build_pdf_args(text)
        if any(k in lower for k in ["extract", "to text", "pdftotext", "ocr", "提取", "转文字"]):
            input_file = str(pdf_args.get("input_file", "input.pdf"))
            args = {"input_file": input_file}
            args.update(pdf_args)
            return (
                "pdf_to_text",
                args,
                [{"type": "task", "command": f"task extract text from {input_file}"}],
            )
        content = infer_pdf_content(text)
        return (
            "generate_pdf",
            {"content": content, **pdf_args},
            [{"type": "task", "command": f"task generate pdf with content {shlex.quote(content)}"}],
        )

    return ("unknown", {}, [])


def infer_weather_location(text: str, entity_hints: Optional[Dict[str, Any]] = None) -> str:
    hinted = ""
    if isinstance(entity_hints, dict):
        hinted = normalize_location(str(entity_hints.get("location", "")).strip())
    if hinted:
        return hinted
    return infer_weather_location_from_text(text)


def infer_quote_symbols(text: str, entity_hints: Optional[Dict[str, Any]] = None) -> List[str]:
    local_symbols = extract_symbols(text)
    hinted_symbols: Any = []
    if isinstance(entity_hints, dict):
        hinted_symbols = entity_hints.get("symbols", [])
    merged = merge_symbol_hints(local_symbols, hinted_symbols)
    return merged


def infer_quote_targets(text: str, entity_hints: Optional[Dict[str, Any]] = None) -> List[str]:
    symbols = infer_quote_symbols(text, entity_hints=entity_hints)
    if symbols:
        return symbols
    company_names: Any = infer_company_names_from_text(text)
    if isinstance(entity_hints, dict):
        company_names = merge_text_hints(company_names, entity_hints.get("company_names"))
    else:
        company_names = merge_text_hints(company_names)
    return company_names


def load_router_config(path: str) -> RouterConfig:
    with open(path, "r", encoding="utf-8") as handle:
        payload = json.load(handle)

    router = payload.get("router", {}) if isinstance(payload, dict) else {}
    llm = payload.get("llm", {}) if isinstance(payload, dict) else {}

    auth_token = ""
    if isinstance(router, dict):
        auth_token = str(router.get("auth_token", "")).strip()

    llm_api_key = ""
    llm_base_url = ""
    llm_model = "gpt-4.1-mini"
    if isinstance(llm, dict):
        llm_api_key = str(llm.get("api_key", "")).strip()
        llm_base_url = str(llm.get("base_url", "")).strip()
        llm_model = str(llm.get("model", "gpt-4.1-mini")).strip() or "gpt-4.1-mini"

    return RouterConfig(
        auth_token=auth_token,
        llm_api_key=llm_api_key,
        llm_base_url=llm_base_url,
        llm_model=llm_model,
    )


def utc_now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def render_status_page_html(payload: Dict[str, Any]) -> str:
    service = payload.get("service", {}) if isinstance(payload, dict) else {}
    router = payload.get("router", {}) if isinstance(payload, dict) else {}
    endpoints = payload.get("endpoints", {}) if isinstance(payload, dict) else {}

    def h(value: Any) -> str:
        return html.escape(str(value))

    rows = [
        ("Status", payload.get("status", "")),
        ("Service", service.get("name", "")),
        ("Version", service.get("version", "")),
        ("Planner", service.get("planner", "")),
        ("LangChain Available", service.get("langchain_available", "")),
        ("LLM Enabled", service.get("llm_enabled", "")),
        ("Started At (UTC)", payload.get("started_at", "")),
        ("Uptime Seconds", payload.get("uptime_seconds", "")),
        ("Requests Total", router.get("requests_total", "")),
        ("Success Total", router.get("success_total", "")),
        ("Errors Total", router.get("errors_total", "")),
        ("Success Rate", router.get("success_rate", "")),
        ("Last Route At (UTC)", router.get("last_route_at", "")),
        ("Last Route Latency (ms)", router.get("last_route_latency_ms", "")),
        ("Last Skill", router.get("last_skill_id", "")),
        ("Last Intent", router.get("last_intent", "")),
        ("Last Reason", router.get("last_reason", "")),
        ("Last Confidence", router.get("last_confidence", "")),
        ("Route Endpoint", endpoints.get("route", "")),
        ("Status JSON", endpoints.get("status_json", "")),
        ("Health Endpoint", endpoints.get("healthz", "")),
    ]
    row_html = "\n".join(
        f"<tr><th>{h(key)}</th><td>{h(value)}</td></tr>"
        for key, value in rows
    )
    return (
        "<!doctype html><html><head><meta charset='utf-8'>"
        "<title>Longtrade Skill Router Status</title>"
        "<style>body{font-family:ui-monospace,Menlo,Consolas,monospace;margin:24px;background:#f7f7f7;color:#111}"
        "table{border-collapse:collapse;background:white;width:100%;max-width:920px}"
        "th,td{border:1px solid #ddd;padding:8px 10px;text-align:left;font-size:13px}"
        "th{background:#f0f0f0;width:280px}h1{margin:0 0 14px 0;font-size:20px}</style></head><body>"
        "<h1>Longtrade Skill Router Status</h1>"
        f"<table>{row_html}</table>"
        "</body></html>"
    )


def create_app(service: SkillRouterService):
    from fastapi import FastAPI, Header, HTTPException
    from fastapi.responses import HTMLResponse, JSONResponse

    app = FastAPI(title="Longtrade Skill Router", version="2.0.0")

    @app.get("/healthz")
    def healthz() -> Dict[str, str]:
        return {"status": "ok"}

    @app.get("/v1/status")
    def status() -> JSONResponse:
        return JSONResponse(service.status_payload())

    @app.get("/status")
    def status_page() -> HTMLResponse:
        return HTMLResponse(render_status_page_html(service.status_payload()))

    @app.post("/v1/route")
    def route(payload: Dict[str, Any], authorization: Optional[str] = Header(default=None)) -> JSONResponse:
        expected = service.cfg.auth_token.strip()
        if expected:
            given = (authorization or "").strip()
            if not given.startswith("Bearer "):
                raise HTTPException(status_code=401, detail="missing bearer token")
            token = given[len("Bearer ") :].strip()
            if token != expected:
                raise HTTPException(status_code=401, detail="invalid bearer token")

        try:
            result = service.route(payload)
            return JSONResponse(result)
        except Exception as exc:
            return JSONResponse(
                {
                    "skill_id": "",
                    "intent": "",
                    "arguments": {},
                    "confidence": 0,
                    "actions": [],
                    "steps": [],
                    "recommendations": [],
                    "reason": "internal_error",
                    "trace": {"planner": service.planner_backend},
                    "errors": [str(exc)],
                },
                status_code=200,
            )

    return app


def main() -> None:
    parser = argparse.ArgumentParser(description="Longtrade Skill Router sidecar")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=19090)
    parser.add_argument("--config", required=True, help="Path to skills_llm.json V2")
    args = parser.parse_args()

    cfg = load_router_config(args.config)
    service = SkillRouterService(cfg)
    app = create_app(service)

    import uvicorn

    uvicorn.run(app, host=args.host, port=args.port, log_level="info")


class _SimpleGraph:
    def __init__(self, steps, planner_name: str):
        self.steps = steps
        self.planner_name = planner_name

    def invoke(self, state: RouteState) -> RouteState:
        current = state
        for step in self.steps:
            current = step(current)
        trace = dict(current.get("trace", {}) or {})
        trace.setdefault("planner", self.planner_name)
        current["trace"] = trace
        return current


if __name__ == "__main__":
    main()

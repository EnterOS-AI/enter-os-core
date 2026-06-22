// @vitest-environment jsdom
/**
 * Tests for FlightEnvelope — the envelope that animates from `from` to `to`.
 *
 * Locks the render contract the canvas + concierge-home both depend on:
 *  - the envelope is positioned at the `from` point (its launch anchor),
 *  - it is coloured by activity kind,
 *  - it degrades gracefully when Element.animate is unavailable (jsdom / SSR).
 *
 * The grow→shrink scale arc itself uses the Web Animations API, which jsdom
 * does not implement, so we assert the static render + graceful degradation
 * rather than keyframe values.
 */
import React from "react";
import { render, cleanup } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { FlightEnvelope } from "../FlightEnvelope";

afterEach(cleanup);

describe("FlightEnvelope", () => {
  it("positions the envelope at the `from` launch point", () => {
    const { getByTestId } = render(
      <FlightEnvelope from={{ x: 120, y: 240 }} to={{ x: 400, y: 60 }} kind="send" />,
    );
    const el = getByTestId("flight-envelope");
    expect(el.style.left).toBe("120px");
    expect(el.style.top).toBe("240px");
    expect(el.querySelector("svg")).toBeTruthy();
  });

  it("colours the envelope by activity kind", () => {
    const stroke = (kind: "send" | "receive" | "task") => {
      const { container } = render(
        <FlightEnvelope from={{ x: 0, y: 0 }} to={{ x: 10, y: 10 }} kind={kind} />,
      );
      const s = container.querySelector("rect")?.getAttribute("stroke");
      cleanup();
      return s;
    };
    expect(stroke("send")).toBe("#22d3ee");
    expect(stroke("receive")).toBe("#8b5cf6");
    expect(stroke("task")).toBe("#f5a623");
  });

  it("degrades to a static render (no throw) when Element.animate is unavailable", () => {
    // jsdom does not implement Element.animate — the component must still render.
    expect(typeof document.createElement("div").animate).not.toBe("function");
    const { getByTestId } = render(
      <FlightEnvelope from={{ x: 0, y: 0 }} to={{ x: 1, y: 1 }} kind="task" />,
    );
    expect(getByTestId("flight-envelope")).toBeTruthy();
  });
});

describe("EndpointBounce (sender flick + receiver catch)", () => {
  it("renders a bounce element at BOTH endpoints (sender + receiver)", () => {
    const { getAllByTestId } = render(
      <FlightEnvelope from={{ x: 10, y: 20 }} to={{ x: 300, y: 400 }} kind="send" />,
    );
    const bounces = getAllByTestId("flight-endpoint-bounce");
    expect(bounces).toHaveLength(2);
    expect(bounces[0].style.left).toBe("10px");
    expect(bounces[0].style.top).toBe("20px");
    expect(bounces[1].style.left).toBe("300px");
    expect(bounces[1].style.top).toBe("400px");
  });

  it("bounce dots/rings start invisible (WAAPI-unavailable degrade = nothing visible)", () => {
    const { getAllByTestId } = render(
      <FlightEnvelope from={{ x: 0, y: 0 }} to={{ x: 5, y: 5 }} kind="receive" />,
    );
    for (const el of getAllByTestId("flight-endpoint-bounce")) {
      const dot = el.querySelector("[data-bounce-dot]");
      const ring = el.querySelector("[data-bounce-ring]");
      expect(dot?.getAttribute("opacity")).toBe("0");
      expect(ring?.getAttribute("opacity")).toBe("0");
    }
  });

  it("bounce colour matches the flight kind", () => {
    const { getAllByTestId } = render(
      <FlightEnvelope from={{ x: 0, y: 0 }} to={{ x: 5, y: 5 }} kind="task" />,
    );
    const dot = getAllByTestId("flight-endpoint-bounce")[0].querySelector("[data-bounce-dot]");
    expect(dot?.getAttribute("fill")).toBe("#f5a623");
  });
});

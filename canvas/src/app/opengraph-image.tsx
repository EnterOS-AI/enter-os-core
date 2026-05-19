import { ImageResponse } from "next/og";

// Marketing-launch SEO (mc#1486). Next.js App-Router file-system OG
// convention: served as `/opengraph-image` and auto-attached as
// `og:image` + `twitter:image`. Dynamic (not a static PNG in /public)
// so we can iterate the brand mark + tagline pre-launch without
// churning a binary blob in git history.
export const runtime = "edge";

export const alt = "Molecule AI — the AI org chart canvas";
export const size = { width: 1200, height: 630 };
export const contentType = "image/png";

export default function OG() {
  return new ImageResponse(
    (
      <div
        style={{
          width: "100%",
          height: "100%",
          display: "flex",
          flexDirection: "column",
          alignItems: "flex-start",
          justifyContent: "center",
          padding: "80px",
          background:
            "linear-gradient(135deg, #0a0a0a 0%, #1a1a2e 60%, #16213e 100%)",
          color: "#ffffff",
          fontFamily: "system-ui, -apple-system, sans-serif",
        }}
      >
        <div
          style={{
            fontSize: 28,
            color: "#a3a3c2",
            letterSpacing: "0.18em",
            textTransform: "uppercase",
            marginBottom: 24,
          }}
        >
          Molecule AI
        </div>
        <div
          style={{
            fontSize: 76,
            fontWeight: 700,
            lineHeight: 1.05,
            letterSpacing: "-0.02em",
            maxWidth: 980,
          }}
        >
          The AI org chart canvas
        </div>
        <div
          style={{
            fontSize: 32,
            color: "#c8c8d8",
            marginTop: 32,
            lineHeight: 1.3,
            maxWidth: 980,
          }}
        >
          Wire Claude Code, Codex, Hermes, and OpenClaw agents into a governed
          multi-agent workspace.
        </div>
        <div
          style={{
            position: "absolute",
            right: 80,
            bottom: 80,
            fontSize: 22,
            color: "#7a7a96",
            display: "flex",
          }}
        >
          moleculesai.app
        </div>
      </div>
    ),
    { ...size },
  );
}

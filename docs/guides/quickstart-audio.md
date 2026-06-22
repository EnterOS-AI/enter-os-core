---
title: "EnterOS Quick Start — Audio Guide"
description: "Audio walkthrough of the EnterOS quick start — from platform setup to your first agent on the canvas."
tags: [onboarding, quickstart, audio]
---

## TTS Script

*Target: 65–75 seconds, en-US-AriaNeural*

---

Getting started with EnterOS takes about five minutes.

First, clone the repo and run the setup script. It boots Postgres, Redis, Langfuse, and Temporal — everything the platform needs to run.

Then start the workspace server on port 8080, and the canvas UI on port 3000. Open your browser to localhost 3000.

You land on the canvas — an empty org. The first thing to do is deploy a template. Pick LangGraph, Claude Code, CrewAI — or start blank. The template provisions a workspace and puts it on the canvas.

Open the chat tab. Send the agent a task. Watch it work.

Now the part that matters: the org structure. Workspaces nest. A PM workspace can have researcher, QA, and data-pipeline workspaces as children. The PM dispatches tasks; the children report back. That's A2A — the agent-to-agent protocol that makes multi-agent orchestration work.

When you're ready to expand: hit expand, pick a template, the platform provisions the workspace, it registers and appears on the canvas. You govern from one place.

That's EnterOS. One canvas. One org chart. One audit trail. Agents that run anywhere.

Find the full docs at moleculesai dot A I.
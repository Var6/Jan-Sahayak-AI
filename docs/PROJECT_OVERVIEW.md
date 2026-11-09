# Jan Sahayak AI — Project Overview

Jan Sahayak AI is a voice-first, offline-first kiosk that helps citizens in
Bihar understand government welfare schemes and public services: pensions,
scholarships, farmer support, loans and subsidies, certificates, ration
cards, housing, health insurance and self-employment schemes.

A citizen presses a large button, speaks in Hindi or Hinglish (or types),
and the kiosk:

1. understands what they need ("मुझे लोन चाहिए", "जाति प्रमाण पत्र बनवाना है"),
2. asks the minimum clarifying questions,
3. explains eligibility, benefits, documents and how to apply, using only
   records verified by Janman reviewers against official sources,
4. points to official application channels (RTPS counter, CSC, block
   office, official portals),
5. says clearly when information still needs verification, and never
   guarantees approval.

It is **not** a general chatbot and **not** an official government service.
It runs its speech recognition, speech synthesis and language model locally,
so it works without internet; anonymous usage statistics sync to a central
server when a connection is available.

## Who it is for

People with limited digital literacy, visiting public places such as block
offices, panchayat bhawans and community centres. Design priorities: Hindi
first, large touch targets, spoken answers with on-screen text, short
sessions, no personal data collection.

## Components

* **Kiosk device** (Raspberry Pi 5 in production; Mac for development): Go
  device agent, local AI runtimes (sherpa-onnx speech, llama.cpp LLM), React
  kiosk UI in Chromium kiosk mode, SQLite.
* **Central server**: Go API + PostgreSQL, admin dashboard (Next.js static).

See `SYSTEM_ARCHITECTURE.md` for the design and `PROGRESS.md` for status.

Built for Janman People's Foundation.

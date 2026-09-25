#!/usr/bin/env python3
"""Smoke-test cpa-groq through a running CLIProxyAPI instance.

Sends a generated one-second sine tone (no recorded speech) to both models via
generateContent and streamGenerateContent, and checks the Gemini response
shape. A tone usually transcribes to an empty or near-empty string; the check
is about the path, not the words.

Usage:
    CPA_API_KEY=... scripts/smoke.py [--base http://127.0.0.1:8317] [--audio file --mime audio/ogg]

The client key is read from the CPA_API_KEY environment variable and is never
printed. Standard library only.
"""
import argparse
import base64
import io
import json
import math
import os
import struct
import sys
import time
import urllib.error
import urllib.request
import wave

MODELS = ["groq-whisper-large-v3", "groq-whisper-large-v3-turbo"]


def tone_wav(seconds=1.0, freq=440.0, rate=16000) -> bytes:
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(rate)
        frames = b"".join(
            struct.pack("<h", int(8000 * math.sin(2 * math.pi * freq * i / rate)))
            for i in range(int(seconds * rate))
        )
        w.writeframes(frames)
    return buf.getvalue()


def call(base, key, path, body, timeout):
    req = urllib.request.Request(base.rstrip("/") + path, data=body, method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("x-goog-api-key", key)
    started = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read(), time.perf_counter() - started
    except urllib.error.HTTPError as err:
        return err.code, err.read(), time.perf_counter() - started


def check_gemini(obj):
    cand = obj["candidates"][0]
    assert cand["content"]["role"] == "model", "role"
    assert isinstance(cand["content"]["parts"][0]["text"], str), "text"
    assert cand["finishReason"] == "STOP", "finishReason"
    usage = obj["usageMetadata"]
    assert usage["totalTokenCount"] == usage["promptTokenCount"] + usage["candidatesTokenCount"], "usage"
    return cand["content"]["parts"][0]["text"]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:8317")
    ap.add_argument("--audio", help="optional audio file instead of the generated tone")
    ap.add_argument("--mime", default="audio/wav")
    ap.add_argument("--timeout", type=float, default=120)
    args = ap.parse_args()
    key = os.environ.get("CPA_API_KEY", "").strip()
    if not key:
        sys.exit("set CPA_API_KEY")
    audio = open(args.audio, "rb").read() if args.audio else tone_wav()
    body = json.dumps({"contents": [{"role": "user", "parts": [
        {"inline_data": {"mime_type": args.mime, "data": base64.b64encode(audio).decode()}}]}]}).encode()

    failures = 0
    for model in MODELS:
        for method, suffix in (("generateContent", ""), ("streamGenerateContent", "?alt=sse")):
            status, raw, took = call(args.base, key, f"/v1beta/models/{model}:{method}{suffix}", body, args.timeout)
            label = f"{model}:{method}"
            try:
                if status != 200:
                    raise AssertionError(f"HTTP {status}: {raw[:300].decode('utf-8', 'replace')}")
                if suffix:
                    events = [json.loads(line[6:]) for line in raw.decode().splitlines() if line.startswith("data: ")]
                    assert len(events) == 1, f"expected 1 SSE event, got {len(events)}"
                    text = check_gemini(events[0])
                else:
                    text = check_gemini(json.loads(raw))
                print(f"ok    {label:52} {took * 1000:7.0f} ms  transcript chars={len(text)}")
            except (AssertionError, KeyError, IndexError, ValueError) as err:
                failures += 1
                print(f"FAIL  {label:52} {took * 1000:7.0f} ms  {err}")
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()

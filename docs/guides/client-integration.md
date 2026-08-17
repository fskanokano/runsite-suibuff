# Client Integration Guide

Connect any OpenAI-compatible AI tool or coding assistant to `freebuff-proxy`.

**Server Base URL:** `http://localhost:3457/v1`  
**API Key:** `not-needed` (or your FreeBuff token in bridge mode)

Any OpenAI-compatible client works: OpenCode, pi, 9router, LiteLLM, or your own scripts (Python / Node.js below).

---

## Bridge Mode vs Pooled Mode

+ **Pooled Mode (Default):** Set `AUTH_TOKENS=token1,token2` in the proxy's `.env`. The proxy drains keys one at a time: it prefers the token with a live session and only moves on when one is rate-limited, never aggressively rotating healthy keys. Clients can use any placeholder API key.
+ **Bridge Mode (Routers & Multi-User):** Leave `AUTH_TOKENS=` empty in `.env`. The proxy acts as a zero-storage relay. **API Routers ([9router](9router-integration.md), OmniRouter, One API, LiteLLM) send actual FreeBuff tokens in `Authorization: Bearer <freebuff-token>`.** The proxy lazily creates sessions per client token with LRU caching, rate limits, and health tracking; cached bridge entries are visible in `GET /healthz`.
---

## 1. opencode

Add to `opencode.json` or `~/.config/opencode/opencode.json`:

```json
{
  "providers": {
    "freebuff": {
      "type": "openai",
      "options": {
        "baseURL": "http://localhost:3457/v1",
        "apiKey": "not-needed"
      },
      "models": [
        { "id": "deepseek/deepseek-v4-flash", "name": "DeepSeek Flash" }
      ]
    }
  }
}
```

---

## 2. pi (Coding Agent CLI)

Add a provider to `~/.pi/agent/models.json`:

```json
{
  "providers": {
    "freebuff": {
      "baseUrl": "http://localhost:3457/v1",
      "api": "openai-completions",
      "apiKey": "not-needed",
      "models": [
        { "id": "deepseek/deepseek-v4-flash", "name": "DeepSeek Flash" }
      ]
    }
  }
}
```

Pick the model with `/model` inside pi, or start it directly with `pi --model deepseek/deepseek-v4-flash`. The file is re-read each time you open `/model`, so no restart is needed.

---

## 3. Python (Official OpenAI SDK)

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:3457/v1",
    api_key="not-needed"
)

response = client.chat.completions.create(
    model="deepseek/deepseek-v4-flash",
    messages=[{"role": "user", "content": "Write a python function to check prime numbers."}],
    stream=True
)

for chunk in response:
    if chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="")
```

---

## 4. Node.js (Official OpenAI SDK)

```javascript
import OpenAI from 'openai';

const openai = new OpenAI({
  baseURL: 'http://localhost:3457/v1',
  apiKey: 'not-needed',
});

const response = await openai.chat.completions.create({
  model: 'deepseek/deepseek-v4-flash',
  messages: [{ role: 'user', content: 'Say hello in TypeScript!' }],
  stream: true,
});

for await (const chunk of response) {
  process.stdout.write(chunk.choices[0]?.delta?.content || '');
}
```

---

---

## 5. Cursor IDE

1. Open **Cursor Settings** -> **Models**.
2. Turn off other models and click **Add custom model**: `deepseek/deepseek-v4-flash`.
3. In **OpenAI API Key**, enter any placeholder (e.g. `not-needed`).
4. Click **Override OpenAI Base URL** and set: `http://localhost:3457/v1`.

---

## 6. VS Code (Continue / Cline / Roo Code)

### Continue Extension (`~/.continue/config.json`)
```json
{
  "models": [
    {
      "title": "FreeBuff DeepSeek Flash",
      "provider": "openai",
      "model": "deepseek/deepseek-v4-flash",
      "apiBase": "http://localhost:3457/v1",
      "apiKey": "not-needed"
    }
  ]
}
```

### Cline / Roo Code
- **API Provider**: OpenAI Compatible
- **Base URL**: `http://localhost:3457/v1`
- **API Key**: `not-needed`
- **Model ID**: `deepseek/deepseek-v4-flash`

---

## 7. Chatbox / NextChat / LibreChat / Jan

- **API Host / Base URL**: `http://localhost:3457/v1`
- **API Key**: `not-needed`
- **Model Name**: `deepseek/deepseek-v4-flash`

---

## 8. API Routers & Aggregators (9router, OmniRouter, One API, LiteLLM)

For multi-account management or multi-user API routing:

1. **Proxy Setup:** Run the proxy in **Bridge Mode** (leave `AUTH_TOKENS=` empty in `.env`).
2. **Router Setup (9router / OmniRouter):**
   + **Provider Type:** OpenAI Compatible
   + **Base URL:** `http://localhost:3457/v1` (or container host `http://host.docker.internal:3457/v1`)
   + **API Keys:** Add your actual **auth token(s)** as the node API keys in 9router or OmniRouter.
   + **Connection strategy:** with several keys, configure **fallback / priority (fill the first)**: never round-robin, which burns every account's quota at once and is a high-risk signal for account bans (see the [9router guide](9router-integration.md)).
3. **Routing Behavior:** When 9router or OmniRouter routes a request, it sends the key as `Authorization: Bearer <token>`. The proxy lazily creates and caches upstream free sessions for each token without saving any token to disk.

---

## Default model

`deepseek/deepseek-v4-flash` is the default. It is the most open model across all regions and tiers, which is why every example in this guide uses it.

Only request models your account's tier and region actually offers: out-of-tier picks are refused or silently downgraded to `deepseek/deepseek-v4-flash`, and the requested model id is correlated with your egress IP's region.

Query `http://localhost:3457/v1/models` for the full live catalog.

---

## Related docs

- [Getting Started](getting-started.md): 5-minute setup walkthrough
- [9router Integration](9router-integration.md): wiring the proxy into 9router
- [README](../../README.md): overview, config reference, quick start

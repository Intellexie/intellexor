package provider

const fallbackCatalog = `{
  "providers": {
    "anthropic": {
      "name": "Anthropic",
      "env": ["ANTHROPIC_API_KEY"],
      "npm": "@ai-sdk/anthropic",
      "models": {
        "claude-opus-4-2": {"name": "Claude Opus 4", "tool_call": true, "reasoning": true, "attachment": true, "cost": {"input": 15, "output": 75, "cache_read": 1.5, "cache_write": 18.75}, "limit": {"context": 200000, "output": 32000}},
        "claude-sonnet-4-5": {"name": "Claude Sonnet 4.5", "tool_call": true, "reasoning": true, "attachment": true, "cost": {"input": 3, "output": 15, "cache_read": 0.3, "cache_write": 3.75}, "limit": {"context": 200000, "output": 32000}},
        "claude-sonnet-4": {"name": "Claude Sonnet 4", "tool_call": true, "reasoning": true, "attachment": true, "cost": {"input": 3, "output": 15, "cache_read": 0.3, "cache_write": 3.75}, "limit": {"context": 200000, "output": 32000}},
        "claude-haiku-4-5": {"name": "Claude Haiku 4.5", "tool_call": true, "reasoning": true, "attachment": true, "cost": {"input": 0.8, "output": 4, "cache_read": 0.08, "cache_write": 1}, "limit": {"context": 200000, "output": 32000}}
      }
    },
    "openai": {
      "name": "OpenAI",
      "env": ["OPENAI_API_KEY"],
      "npm": "@ai-sdk/openai",
      "models": {
        "gpt-5": {"name": "GPT-5", "tool_call": true, "reasoning": true, "attachment": true, "cost": {"input": 1.25, "output": 10}, "limit": {"context": 400000, "output": 128000}},
        "gpt-5-mini": {"name": "GPT-5 Mini", "tool_call": true, "reasoning": true, "attachment": true, "cost": {"input": 0.25, "output": 2}, "limit": {"context": 400000, "output": 128000}},
        "gpt-4.1": {"name": "GPT-4.1", "tool_call": true, "attachment": true, "cost": {"input": 2, "output": 8}, "limit": {"context": 1000000, "output": 32768}},
        "gpt-4.1-mini": {"name": "GPT-4.1 Mini", "tool_call": true, "attachment": true, "cost": {"input": 0.4, "output": 1.6}, "limit": {"context": 1000000, "output": 32768}},
        "gpt-4.1-nano": {"name": "GPT-4.1 Nano", "tool_call": true, "attachment": true, "cost": {"input": 0.1, "output": 0.4}, "limit": {"context": 1000000, "output": 32768}},
        "o4-mini": {"name": "GPT-5 o4-mini", "tool_call": true, "reasoning": true, "attachment": true, "cost": {"input": 1.1, "output": 4.4}, "limit": {"context": 200000, "output": 100000}}
      }
    },
    "google": {
      "name": "Google",
      "env": ["GEMINI_API_KEY", "GOOGLE_API_KEY"],
      "npm": "@ai-sdk/google",
      "models": {
        "gemini-2.5-pro": {"name": "Gemini 2.5 Pro", "tool_call": true, "reasoning": true, "attachment": true, "cost": {"input": 1.25, "output": 10}, "limit": {"context": 1048576, "output": 65536}},
        "gemini-2.5-flash": {"name": "Gemini 2.5 Flash", "tool_call": true, "reasoning": true, "attachment": true, "cost": {"input": 0.3, "output": 2.5}, "limit": {"context": 1048576, "output": 65536}},
        "gemini-2.5-flash-lite": {"name": "Gemini 2.5 Flash Lite", "tool_call": true, "attachment": true, "cost": {"input": 0.1, "output": 0.4}, "limit": {"context": 1048576, "output": 65536}}
      }
    },
    "openrouter": {
      "name": "OpenRouter",
      "env": ["OPENROUTER_API_KEY"],
      "npm": "@openrouter/ai-sdk-provider",
      "url": "https://openrouter.ai/api/v1",
      "models": {
        "anthropic/claude-sonnet-4.5": {"name": "OpenRouter Claude Sonnet 4.5", "tool_call": true, "reasoning": true, "attachment": true, "cost": {"input": 3, "output": 15}, "limit": {"context": 200000, "output": 32000}},
        "openai/gpt-5": {"name": "OpenRouter GPT-5", "tool_call": true, "reasoning": true, "attachment": true, "cost": {"input": 1.25, "output": 10}, "limit": {"context": 400000, "output": 128000}},
        "google/gemini-2.5-flash": {"name": "OpenRouter Gemini 2.5 Flash", "tool_call": true, "reasoning": true, "attachment": true, "cost": {"input": 0.3, "output": 2.5}, "limit": {"context": 1048576, "output": 65536}}
      }
    },
    "deepseek": {
      "name": "DeepSeek",
      "env": ["DEEPSEEK_API_KEY"],
      "npm": "@ai-sdk/openai-compatible",
      "url": "https://api.deepseek.com/v1",
      "models": {
        "deepseek-chat": {"name": "DeepSeek Chat", "tool_call": true, "cost": {"input": 0.27, "output": 1.1}, "limit": {"context": 256000, "output": 8192}},
        "deepseek-reasoner": {"name": "DeepSeek Reasoner", "tool_call": true, "reasoning": true, "cost": {"input": 0.55, "output": 2.19}, "limit": {"context": 256000, "output": 65536}}
      }
    },
    "xai": {
      "name": "xAI",
      "env": ["XAI_API_KEY"],
      "npm": "@ai-sdk/xai",
      "url": "https://api.x.ai/v1",
      "models": {
        "grok-4": {"name": "Grok 4", "tool_call": true, "reasoning": true, "attachment": true, "cost": {"input": 3, "output": 15}, "limit": {"context": 256000, "output": 65536}},
        "grok-3-mini": {"name": "Grok 3 Mini", "tool_call": true, "reasoning": true, "cost": {"input": 0.3, "output": 0.5}, "limit": {"context": 131072, "output": 65536}}
      }
    },
    "groq": {
      "name": "Groq",
      "env": ["GROQ_API_KEY"],
      "npm": "@ai-sdk/groq",
      "url": "https://api.groq.com/openai/v1",
      "models": {
        "llama-3.3-70b-versatile": {"name": "Llama 3.3 70B", "tool_call": true, "cost": {"input": 0.59, "output": 0.79}, "limit": {"context": 131072, "output": 32768}},
        "deepseek-r1-distill-llama-70b": {"name": "DeepSeek R1 Distill Llama 70B", "tool_call": true, "reasoning": true, "cost": {"input": 0.75, "output": 0.99}, "limit": {"context": 131072, "output": 32768}}
      }
    },
    "mistral": {
      "name": "Mistral",
      "env": ["MISTRAL_API_KEY"],
      "npm": "@ai-sdk/mistral",
      "url": "https://api.mistral.ai/v1",
      "models": {
        "mistral-large-latest": {"name": "Mistral Large", "tool_call": true, "cost": {"input": 2, "output": 6}, "limit": {"context": 128000, "output": 8192}}
      }
    },
    "ollama": {
      "name": "Ollama",
      "env": [],
      "npm": "@ai-sdk/openai-compatible",
      "url": "http://127.0.0.1:11434/v1",
      "models": {
        "llama3.1": {"name": "Llama 3.1 (local)", "tool_call": true, "cost": {"input": 0, "output": 0}, "limit": {"context": 131072, "output": 8192}},
        "qwen2.5-coder": {"name": "Qwen2.5 Coder (local)", "tool_call": true, "cost": {"input": 0, "output": 0}, "limit": {"context": 32768, "output": 8192}},
        "deepseek-r1": {"name": "DeepSeek R1 (local)", "tool_call": true, "reasoning": true, "cost": {"input": 0, "output": 0}, "limit": {"context": 131072, "output": 8192}}
      }
    },
    "lmstudio": {
      "name": "LM Studio",
      "env": [],
      "npm": "@ai-sdk/openai-compatible",
      "url": "http://localhost:1234/v1",
      "models": {
        "local-model": {"name": "LM Studio (any loaded)", "tool_call": true, "cost": {"input": 0, "output": 0}, "limit": {"context": 32768, "output": 8192}}
      }
    }
  }
}`

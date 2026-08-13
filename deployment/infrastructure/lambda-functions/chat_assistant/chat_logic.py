# ABOUTME: Core chat assistant logic - invokes Amazon Bedrock to generate chat responses
# ABOUTME: Uses the Bedrock Converse API (bedrock:Converse) following the project's Bedrock usage patterns

import os
import boto3
from botocore.config import Config

# Configuration from environment
BEDROCK_MODEL_ID = os.environ.get(
    "BEDROCK_MODEL_ID", "anthropic.claude-3-5-sonnet-20241022-v2:0"
)
BEDROCK_REGION = os.environ.get("BEDROCK_REGION", os.environ.get("AWS_REGION", "us-east-1"))
MAX_TOKENS = int(os.environ.get("MAX_TOKENS", "1024"))
TEMPERATURE = float(os.environ.get("TEMPERATURE", "0.7"))
SYSTEM_PROMPT = os.environ.get(
    "SYSTEM_PROMPT",
    "You are a helpful assistant for the Claude Code with Amazon Bedrock project. "
    "Answer questions concisely and accurately.",
)

# Reuse the Bedrock Runtime client across warm invocations
_bedrock_config = Config(region_name=BEDROCK_REGION, retries={"max_attempts": 3, "mode": "adaptive"})
bedrock_runtime = boto3.client("bedrock-runtime", config=_bedrock_config)


class ChatAssistantError(Exception):
    """Raised when the chat assistant fails to generate a response."""


def get_chat_response(message: str, session_id: str = None, userId: str = None, history: list | None = None) -> dict:
    """
    Generate a chat response for the given user message using Amazon Bedrock.

    Args:
        message: The user's chat message (required).
        session_id: Optional session identifier used for conversation continuity/logging.
        userId: Optional user identifier used for logging/attribution.
        history: Optional list of prior conversation turns, each a dict with
            "role" ("user" or "assistant") and "content" (str).

    Returns:
        dict with keys: "reply" (str), "sessionId" (str), "modelId" (str), "usage" (dict)

    Raises:
        ChatAssistantError: if the message is invalid or Bedrock invocation fails.
    """
    if not message or not isinstance(message, str) or not message.strip():
        raise ChatAssistantError("A non-empty 'message' field is required")

    messages = []
    for turn in history or []:
        role = turn.get("role")
        content = turn.get("content")
        if role in ("user", "assistant") and content:
            messages.append({"role": role, "content": [{"text": str(content)}]})

    messages.append({"role": "user", "content": [{"text": message}]})

    try:
        response = bedrock_runtime.converse(
            modelId=BEDROCK_MODEL_ID,
            messages=messages,
            system=[{"text": SYSTEM_PROMPT}],
            inferenceConfig={
                "maxTokens": MAX_TOKENS,
                "temperature": TEMPERATURE,
            },
        )
    except Exception as e:
        raise ChatAssistantError(f"Bedrock invocation failed: {str(e)}") from e

    try:
        output_message = response["output"]["message"]
        reply_text = "".join(
            block.get("text", "") for block in output_message.get("content", []) if "text" in block
        )
    except (KeyError, IndexError) as e:
        raise ChatAssistantError(f"Unexpected Bedrock response format: {str(e)}") from e

    usage = response.get("usage", {})

    return {
        "reply": reply_text,
        "sessionId": session_id,
        "userId": userId,
        "modelId": BEDROCK_MODEL_ID,
        "usage": {
            "inputTokens": usage.get("inputTokens"),
            "outputTokens": usage.get("outputTokens"),
            "totalTokens": usage.get("totalTokens"),
        },
    }

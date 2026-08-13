# ABOUTME: Lambda handler for the chat assistant API - proxies chat requests to Amazon Bedrock
# ABOUTME: Accepts API Gateway (HTTP/REST) events, extracts message/sessionId/userId, returns JSON response

import json
import logging
import os

from chat_logic import ChatAssistantError, get_chat_response

# Configure logging for CloudWatch
logger = logging.getLogger()
logger.setLevel(os.environ.get("LOG_LEVEL", "INFO"))

# CORS configuration (mirrors patterns used by quota_check/quota_monitor/sidecar_monitor)
ALLOWED_ORIGIN = os.environ.get("ALLOWED_ORIGIN", "*")
CORS_HEADERS = {
    "Content-Type": "application/json",
    "Access-Control-Allow-Origin": ALLOWED_ORIGIN,
    "Access-Control-Allow-Methods": "POST, OPTIONS",
    "Access-Control-Allow-Headers": "Content-Type, Authorization",
}


def build_response(status_code: int, body: dict) -> dict:
    """Build an API Gateway response with CORS headers."""
    return {
        "statusCode": status_code,
        "headers": CORS_HEADERS,
        "body": json.dumps(body),
    }


def _extract_request_body(event: dict) -> dict:
    """Extract and parse the JSON body from an API Gateway event.

    Supports both REST API (v1) and HTTP API (v2) event formats.
    """
    raw_body = event.get("body")

    if raw_body is None:
        return {}

    if event.get("isBase64Encoded"):
        import base64

        raw_body = base64.b64decode(raw_body).decode("utf-8")

    if isinstance(raw_body, dict):
        return raw_body

    if not raw_body:
        return {}

    try:
        parsed = json.loads(raw_body)
        return parsed if isinstance(parsed, dict) else {}
    except (json.JSONDecodeError, TypeError) as e:
        logger.warning("Failed to parse request body as JSON: %s", e)
        raise ValueError("Request body must be valid JSON")


def lambda_handler(event, context):
    """
    Lambda entrypoint for the chat assistant API.

    Expected input (via API Gateway):
        HTTP POST with a JSON body: {"message": str, "sessionId": str, "userId": str}

    Returns:
        API Gateway-compatible response dict with statusCode, headers, and JSON body.
    """
    request_id = getattr(context, "aws_request_id", None)
    http_method = (
        event.get("httpMethod")
        or event.get("requestContext", {}).get("http", {}).get("method")
        or "POST"
    )

    logger.info("chat_assistant invoked - request_id=%s method=%s", request_id, http_method)

    # Handle CORS preflight requests
    if http_method == "OPTIONS":
        return build_response(200, {"message": "OK"})

    try:
        body = _extract_request_body(event)
    except ValueError as e:
        logger.warning("Invalid request body: %s", e)
        return build_response(400, {"error": "invalid_request", "message": str(e)})

    message = body.get("message")
    session_id = body.get("sessionId")
    user_id = body.get("userId")
    history = body.get("history")

    if not message:
        logger.warning("Missing 'message' field in request - request_id=%s", request_id)
        return build_response(400, {
            "error": "missing_message",
            "message": "The 'message' field is required in the request body",
        })

    try:
        result = get_chat_response(
            message=message,
            session_id=session_id,
            userId=user_id,
            history=history,
        )

        logger.info(
            "chat_assistant success - request_id=%s sessionId=%s userId=%s tokens=%s",
            request_id,
            session_id,
            user_id,
            result.get("usage"),
        )

        return build_response(200, {
            "reply": result["reply"],
            "sessionId": result["sessionId"],
            "userId": result["userId"],
            "modelId": result["modelId"],
            "usage": result["usage"],
        })

    except ChatAssistantError as e:
        logger.error("Chat assistant error - request_id=%s error=%s", request_id, str(e))
        return build_response(502, {
            "error": "chat_assistant_error",
            "message": str(e),
        })

    except Exception as e:
        logger.exception("Unhandled error in chat_assistant - request_id=%s", request_id)
        return build_response(500, {
            "error": "internal_error",
            "message": f"An unexpected error occurred: {str(e)}",
        })

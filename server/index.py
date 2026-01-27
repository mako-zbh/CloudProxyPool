# -*- coding: utf8 -*-
import json
import base64
import urllib.request
import urllib.error
import urllib.parse
import ssl

def main_handler(event, context):
    """
    Tencent Cloud SCF Main Handler via Function URL
    Dependencies: None (Standard Lib only)
    """
    try:
        # Parse Request Body
        raw_body = event.get('body', "")
        if event.get('isBase64Encoded', False):
            raw_body = base64.b64decode(raw_body).decode('utf-8')
            
        if not raw_body:
            return mk_response(400, {"error": "Empty Request Body"})

        try:
            data = json.loads(raw_body)
        except json.JSONDecodeError:
            return mk_response(400, {"error": "Invalid JSON"})
        
        method = data.get('method', 'GET')
        url = data.get('url', '')
        headers = data.get('headers', {})
        body_content = data.get('body', '')
        
        if not url:
            return mk_response(400, {"error": "Missing URL"})

        # Prepare payload
        payload = None
        if body_content:
            if data.get('is_body_base64', False):
                payload = base64.b64decode(body_content)
            else:
                payload = body_content.encode('utf-8')

        # Create Request
        req = urllib.request.Request(url, data=payload, headers=headers, method=method)
        
        # Context: Ignore SSL errors for target
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE

        try:
            with urllib.request.urlopen(req, timeout=10, context=ctx) as resp:
                content = resp.read()
                return mk_proxy_response(resp.getcode(), dict(resp.info()), content)
                
        except urllib.error.HTTPError as e:
            # Forward HTTP Errors (404, 500 from target) as successful proxy response
            content = e.read()
            return mk_proxy_response(e.code, dict(e.headers), content)
            
        except urllib.error.URLError as e:
             return mk_response(502, {"error": f"Upstream Error: {e.reason}"})

    except Exception as e:
        return mk_response(500, {"error": f"Internal Server Error: {str(e)}"})

def mk_proxy_response(status, headers, content_bytes):
    """Encapsulates the target response into JSON for the client"""
    return mk_response(200, {
        "status_code": status,
        "headers": headers,
        "content": base64.b64encode(content_bytes).decode('utf-8'),
        "is_content_base64": True
    })

def mk_response(status, body_dict):
    """Standard SCF Response"""
    return {
        "isBase64Encoded": False,
        "statusCode": status,
        "headers": {"Content-Type": "application/json"},
        "body": json.dumps(body_dict)
    }

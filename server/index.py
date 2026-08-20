# -*- coding: utf8 -*-
import os
import hmac
import json
import base64
import ipaddress
import urllib.request
import urllib.error
import urllib.parse
import ssl

# 鉴权 Token，由部署脚本通过环境变量下发；为空表示未启用鉴权
AUTH_TOKEN = os.environ.get("AUTH_TOKEN", "")

def main_handler(event, context):
    """
    Tencent Cloud SCF Main Handler via Function URL
    Dependencies: None (Standard Lib only)
    """
    try:
        # 鉴权: 函数 URL 是公网可访问的，必须校验 Token，
        # 否则 URL 泄露后任何人都能把它当免费代理/SSRF 跳板使用
        if AUTH_TOKEN:
            req_headers = event.get('headers') or {}
            token = ""
            for k, v in req_headers.items():
                if str(k).lower() == 'x-auth-token':
                    token = str(v)
                    break
            if not hmac.compare_digest(token, AUTH_TOKEN):
                return mk_response(403, {"error": "Unauthorized"})

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

        # SSRF 防护: 拒绝内网/保留地址 (127.0.0.1、169.254.x.x、10.x 等) 和云元数据服务
        host = (urllib.parse.urlsplit(url).hostname or "").strip().strip('[]').lower()
        if is_blocked_host(host):
            return mk_response(403, {"error": "Target host is not allowed"})

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

def is_blocked_host(host):
    """内网/保留地址一律拒绝，防止函数被用来探测内网或云元数据"""
    if not host:
        return True
    if host in ("localhost", "metadata.tencentyun.com"):
        return True
    try:
        ip = ipaddress.ip_address(host)
    except ValueError:
        return False
    return not ip.is_global

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

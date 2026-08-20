# -*- coding: utf8 -*-
import os
import re
import sys
import json
import zipfile
import time
import argparse
import requests
import toml
import base64
from tencentcloud.common import credential
from tencentcloud.common.profile.client_profile import ClientProfile
from tencentcloud.common.profile.http_profile import HttpProfile
from tencentcloud.common.exception.tencent_cloud_sdk_exception import TencentCloudSDKException
from tencentcloud.scf.v20180416 import scf_client, models

# 默认配置路径
CONFIG_FILE = "deploy.toml"

def load_config():
    """加载配置文件"""
    if not os.path.exists(CONFIG_FILE):
        print(f"[错误] 找不到配置文件: {CONFIG_FILE}")
        print(f"[提示] 请先创建 {CONFIG_FILE} 并填入 SecretId/SecretKey")
        sys.exit(1)

    try:
        with open(CONFIG_FILE, "r", encoding="utf-8") as f:
            return toml.load(f)
    except Exception as e:
        print(f"[错误] 解析配置文件失败: {e}")
        sys.exit(1)

def get_auth_token(conf):
    """获取鉴权 Token，不存在则自动生成并追加到配置文件"""
    token = conf.get('security', {}).get('auth_token', '')
    if token:
        return token

    import secrets
    token = secrets.token_hex(16)
    conf.setdefault('security', {})['auth_token'] = token
    # 追加而非重写，保留原文件内容和注释
    with open(CONFIG_FILE, "a", encoding="utf-8") as f:
        f.write(f"\n[security]\n# 云函数调用鉴权 Token (客户端 config.toml 中须填相同的 token)\nauth_token = \"{token}\"\n")
    print(f"[+] 已自动生成鉴权 Token 并写入 {CONFIG_FILE}")
    return token

def create_zip(source_dir, output_filename):
    """打包服务端代码"""
    print(f"[+] 正在打包代码: {output_filename}")
    with zipfile.ZipFile(output_filename, 'w', zipfile.ZIP_DEFLATED) as zipf:
        for root, dirs, files in os.walk(source_dir):
            for file in files:
                file_path = os.path.join(root, file)
                arcname = os.path.relpath(file_path, source_dir)
                zipf.write(file_path, arcname)

def get_client(region, secret_id, secret_key):
    """获取腾讯云 SCF 客户端"""
    cred = credential.Credential(secret_id, secret_key)
    httpProfile = HttpProfile()
    httpProfile.endpoint = "scf.tencentcloudapi.com"
    clientProfile = ClientProfile()
    clientProfile.httpProfile = httpProfile
    return scf_client.ScfClient(cred, region, clientProfile)

def get_function_status(client, func_name, namespace):
    """查询函数状态，返回 (Status, 失败原因)"""
    req = models.GetFunctionRequest()
    req.FunctionName = func_name
    req.Namespace = namespace
    resp = client.GetFunction(req)
    return resp.Status, getattr(resp, "StatusReasons", None)

def wait_function_active(client, region, func_name, namespace, timeout=180, interval=3):
    """轮询等待函数进入 Active 状态。

    CreateFunction/UpdateFunctionCode 返回成功只代表请求已受理，函数此时处于
    Creating/Updating 状态，直接操作触发器会报:
    FailedOperation: functionInfo Status is Creating, unsupport operate
    """
    deadline = time.time() + timeout
    last_status = None
    while time.time() < deadline:
        try:
            status, reasons = get_function_status(client, func_name, namespace)
            last_status = status
            if status == "Active":
                return True
            if "Failed" in status:  # CreatingFailed / UpdatingFailed 等终态
                print(f"[错误] [{region}] 函数部署失败，状态: {status}, 原因: {reasons}")
                return False
        except TencentCloudSDKException:
            pass  # 刚提交创建后立即查询可能偶发异常，下一轮重试
        time.sleep(interval)
    print(f"[错误] [{region}] 等待函数就绪超时({timeout}s)，最后状态: {last_status}")
    return False

def wait_function_deleted(client, func_name, namespace, timeout=120, interval=3):
    """轮询等待函数删除完成（删除同样是异步操作）"""
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            get_function_status(client, func_name, namespace)
        except TencentCloudSDKException as e:
            if "ResourceNotFound" in str(e):
                return True
        time.sleep(interval)
    return False

def ensure_auth_env(client, region, func_name, namespace, token):
    """确保函数环境变量中的 AUTH_TOKEN 与本地一致（不存在或不一致时更新配置）"""
    req = models.GetFunctionRequest()
    req.FunctionName = func_name
    req.Namespace = namespace
    resp = client.GetFunction(req)

    current = ""
    env = getattr(resp, "Env", None)
    if env and env.Variables:
        for v in env.Variables:
            if v.Key == "AUTH_TOKEN":
                current = v.Value
    if current == token:
        return True

    print(f"[*] [{region}] 正在下发鉴权 Token...")
    upd = models.UpdateFunctionConfigurationRequest()
    upd.FunctionName = func_name
    upd.Namespace = namespace
    upd.Timeout = resp.Timeout
    upd.MemorySize = resp.MemorySize
    upd.Environment = models.Environment()
    auth_var = models.Variable()
    auth_var.Key = "AUTH_TOKEN"
    auth_var.Value = token
    upd.Environment.Variables = [auth_var]
    try:
        client.UpdateFunctionConfiguration(upd)
    except Exception as e:
        print(f"[错误] [{region}] 更新函数配置失败: {e}")
        return False
    # 更新配置同样会让函数进入 Updating 状态
    return wait_function_active(client, region, func_name, namespace)

def deploy_function(client, region, func_name, zip_path, conf, token):
    """部署或更新云函数"""
    namespace = conf['deployment']['namespace']
    handler = "index.main_handler"
    runtime = conf['deployment']['runtime']
    
    with open(zip_path, "rb") as f:
        code_content = f.read()
    
    # 检查函数是否存在
    exists = False
    try:
        req = models.GetFunctionRequest()
        req.FunctionName = func_name
        req.Namespace = namespace
        client.GetFunction(req)
        exists = True
    except TencentCloudSDKException as err:
        if "ResourceNotFound" in str(err):
            exists = False
        else:
            print(f"[错误] [{region}] 获取函数状态失败: {err}")
            return False

    base64_code = base64.b64encode(code_content).decode('utf-8')

    if exists:
        status, _ = get_function_status(client, func_name, namespace)
        if status == "CreateFailed":
            # 函数从未创建成功(如账号未开通 CLS 导致)，属于僵尸函数，删除后走重建流程
            print(f"[*] [{region}] 函数处于 CreateFailed 状态，删除后重建...")
            del_req = models.DeleteFunctionRequest()
            del_req.FunctionName = func_name
            del_req.Namespace = namespace
            try:
                client.DeleteFunction(del_req)
            except TencentCloudSDKException as e:
                print(f"[错误] [{region}] 删除失败状态的函数出错: {e}")
                return False
            if not wait_function_deleted(client, func_name, namespace):
                print(f"[错误] [{region}] 等待函数删除超时")
                return False
            exists = False
        else:
            print(f"[*] [{region}] 函数已存在，正在更新代码...")
            # 上次运行中断可能让函数停留在 Creating/Updating 状态，先等它就绪再更新
            if not wait_function_active(client, region, func_name, namespace):
                return False

    if exists:
        req = models.UpdateFunctionCodeRequest()
        req.FunctionName = func_name
        req.Namespace = namespace
        req.Handler = handler
        req.ZipFile = base64_code
        try:
            client.UpdateFunctionCode(req)
        except Exception as e:
            print(f"[错误] [{region}] 更新代码失败: {e}")
            return False
    else:
        print(f"[*] [{region}] 函数不存在，正在创建...")
        req = models.CreateFunctionRequest()
        req.FunctionName = func_name
        req.Code = models.Code()
        req.Code.ZipFile = base64_code
        req.Handler = handler
        req.Runtime = runtime
        req.Namespace = namespace
        req.Timeout = conf['deployment'].get('time_out', 60)
        req.Environment = models.Environment()
        auth_var = models.Variable()
        auth_var.Key = "AUTH_TOKEN"
        auth_var.Value = token
        req.Environment.Variables = [auth_var]
        try:
            client.CreateFunction(req)
        except Exception as e:
            print(f"[错误] [{region}] 创建函数失败: {e}")
            return False

    # 创建/更新代码都是异步操作，必须等函数回到 Active 才能配置触发器
    if not wait_function_active(client, region, func_name, namespace):
        return False

    # 更新路径的函数没有新环境变量，这里统一确保鉴权 Token 已下发
    return ensure_auth_env(client, region, func_name, namespace, token)

def enable_function_url(client, region, func_name, conf):
    """启用并获取函数 URL (通过创建 HTTP Trigger)"""
    namespace = conf['deployment']['namespace']
    
    print(f"[*] [{region}] 正在配置函数 URL...")
    
    try:
        # 1. 尝试创建 HTTP 触发器
        req = models.CreateTriggerRequest()
        req.FunctionName = func_name
        req.Namespace = namespace
        req.Type = "http"
        req.TriggerName = "http_trigger"
        req.TriggerDesc = json.dumps({
            "AuthType": "NONE",
            "NetConfig": {
                "EnableIntranet": True,
                "EnableExtranet": True
            }
        })
        req.Enable = "OPEN"
        
        try:
            client.CreateTrigger(req)
        except TencentCloudSDKException as err:
            if "ResourceInUse" in str(err) or "exist" in str(err).lower():
                pass # 已经存在，忽略
            else:
                print(f"[错误] [{region}] 创建触发器失败: {err}")
                return None
        
        # 2. 获取触发器详情以提取 URL
        # 增加重试机制，等待触发器生效
        for attempt in range(5):
            time.sleep(3) # 等待生效
            req = models.GetFunctionRequest()
            req.FunctionName = func_name
            req.Namespace = namespace
            resp = client.GetFunction(req)
            
            # Debug log
            # print(f"[Debug] Attempt {attempt+1}: Found {len(resp.Triggers)} triggers")
            
            for trigger in resp.Triggers:
                # print(f"[Debug] Trigger Type: {trigger.Type}")
                if trigger.Type == 'http':
                    desc = json.loads(trigger.TriggerDesc)
                    # 尝试获取 URL
                    # 1. 优先从 NetConfig 中获取公网访问地址
                    if 'NetConfig' in desc and 'ExtranetUrl' in desc['NetConfig']:
                        return desc['NetConfig']['ExtranetUrl']
                        
                    # 2. 兼容旧字段
                    if 'public_url' in desc:
                        return desc['public_url']
                    if 'sub_domain' in desc:
                        # SCF HTTP 触发器通常返回 sub_domain (不带 scheme)
                        # 格式: service-xxxx.region.apigw.tencentcs.com
                        domain = desc['sub_domain']
                        if not domain.startswith("http"):
                            return f"https://{domain}/release/{func_name}"
                        return f"{domain}/release/{func_name}"
            
            print(f"[*] [{region}] 等待触发器生效 (尝试 {attempt+1}/5)...")
                    
        print(f"[-] [{region}] 未找到 HTTP 触发器信息 (已重试 5 次)")
        # 打印所有触发器以便排查
        for trigger in resp.Triggers:
             print(f"[Debug] Found Trigger: Type={trigger.Type}, Desc={trigger.TriggerDesc}")
        return None

    except TencentCloudSDKException as err:
        print(f"[错误] [{region}] 获取函数 URL 失败: {err}")
        return None

def check_health(url, conf, token):
    """对部署好的 URL 进行健康检查"""
    test_url = conf['health_check']['test_url']
    expected_status = conf['health_check']['expected_status']

    print(f"[*] [健康检查] 正在测试代理: {url} -> {test_url}")

    payload = {
        "method": "GET",
        "url": test_url,
        "headers": {"User-Agent": "Deploy-HealthCheck"},
        "body": ""
    }

    try:
        resp = requests.post(url, json=payload, timeout=15,
                             headers={"X-Auth-Token": token} if token else {})
        if resp.status_code == 200:
            # 解析代理返回的包
            data = resp.json()
            if data.get("status_code") == expected_status:
                content = base64.b64decode(data.get("content", "")).decode('utf-8', errors='ignore')
                # 简单截取一点内容展示
                preview = content.strip()[:50].replace("\n", " ")
                print(f"[+] [健康检查] 通过! 代理响应: {data['status_code']}, 返回内容: {preview}...")
                return True
            else:
                print(f"[-] [健康检查] 失败! 目标返回状态码: {data.get('status_code')}")
                if 'error' in data:
                     print(f"    错误详情: {data.get('error')}")
        else:
            print(f"[-] [健康检查] 失败! 云函数请求异常: {resp.status_code}")
            print(f"    响应内容: {resp.text[:200]}") # 打印前200字符
    except Exception as e:
        print(f"[-] [健康检查] 异常: {e}")
        
    return False

def generate_client_config(urls, token):
    """生成客户端配置文件"""
    config_content = f"""[client]
listen_addr = "127.0.0.1:10800"
socks_addr = ":10801"
dashboard_addr = ":8081"
dump = false
debug = false

# 可选配置 (取消注释以启用):
# user = "admin"
# password = "your_password"
# dump_file = "traffic.log"

[cloud]
# 由 deploy.py 自动生成
function_urls = {json.dumps(urls)}
region = "multi-region"
token = "{token}"
"""
    
    # 生成到 ../client/config.toml
    base_dir = os.path.dirname(os.path.abspath(__file__))
    client_dir = os.path.join(os.path.dirname(base_dir), "client")
    config_path = os.path.join(client_dir, "config.toml")
    
    with open(config_path, "w", encoding="utf-8") as f:
        f.write(config_content)
    print(f"\n[+] 客户端配置文件已生成: '{config_path}' (包含 {len(urls)} 个可用节点)")

def parse_args():
    parser = argparse.ArgumentParser(description="Cloud ProxyPool 自动化部署工具")
    sub = parser.add_subparsers(dest="command")

    clean_p = sub.add_parser("clean", help="清理已部署的函数实例")
    clean_p.add_argument("regions", nargs="*",
                         help="要清理的区域，如 ap-shanghai ap-beijing；不填则使用配置文件中的区域")
    clean_p.add_argument("-y", "--yes", action="store_true", help="跳过删除确认")

    return parser.parse_args()

def list_pool_functions(client, namespace, base_name):
    """列出区域内属于本代理池的函数 (基础名或 基础名_N)"""
    pattern = re.compile(rf"{re.escape(base_name)}(_\d+)?$")
    req = models.ListFunctionsRequest()
    req.Namespace = namespace
    req.SearchKey = base_name
    req.Limit = 100
    try:
        resp = client.ListFunctions(req)
    except Exception as e:
        print(f"[错误] 列出函数失败: {e}")
        return []
    return [f.FunctionName for f in (resp.Functions or []) if pattern.fullmatch(f.FunctionName)]

def cmd_clean(conf, args):
    """清理函数实例: 删除指定区域(默认为配置区域)内所有 基础名/基础名_N 的函数"""
    secret_id = conf['tencent']['secret_id']
    secret_key = conf['tencent']['secret_key']
    namespace = conf['deployment']['namespace']
    base_name = conf['deployment']['function_name']
    regions = args.regions or conf['deployment']['regions']

    # 先收集所有区域的目标，统一展示后再删
    plan = []
    for region in regions:
        client = get_client(region, secret_id, secret_key)
        found = list_pool_functions(client, namespace, base_name)
        if found:
            plan.append((region, client, sorted(found)))
        else:
            print(f"[*] [{region}] 无匹配的函数实例")

    if not plan:
        print("[提示] 没有需要清理的实例。")
        return

    print("\n即将删除以下函数实例:")
    for region, _, names in plan:
        for name in names:
            print(f"  - [{region}] {name}")

    if not args.yes:
        answer = input(f"\n确认删除以上 {sum(len(n) for _, _, n in plan)} 个函数? (y/N): ").strip().lower()
        if answer != "y":
            print("已取消。")
            return

    for region, client, names in plan:
        for name in names:
            req = models.DeleteFunctionRequest()
            req.FunctionName = name
            req.Namespace = namespace
            try:
                client.DeleteFunction(req)
                print(f"[删除已提交] [{region}] {name}")
            except TencentCloudSDKException as e:
                print(f"[错误] [{region}] {name} 删除失败: {e}")

    # 等待删除完成
    for region, client, names in plan:
        for name in names:
            if not wait_function_deleted(client, name, namespace):
                print(f"[警告] [{region}] {name} 删除等待超时，请稍后在控制台确认")

    print("\n[完成] 清理结束。注意: client/config.toml 仍指向旧 URL，重新部署后会被覆盖生成。")

def main():
    args = parse_args()

    # 1. 加载配置
    print("=== Cloud ProxyPool 自动化部署工具 ===")
    conf = load_config()

    if args.command == "clean":
        cmd_clean(conf, args)
        return

    secret_id = conf['tencent']['secret_id']
    secret_key = conf['tencent']['secret_key']
    
    if "YOUR_SECRET_ID" in secret_id:
        print("[提示] 请先修改 configuration file 中的密钥信息！")
        sys.exit(1)

    # 2. 获取/生成鉴权 Token
    token = get_auth_token(conf)

    # 3. 打包
    base_dir = os.path.dirname(os.path.abspath(__file__))
    server_dir = os.path.join(os.path.dirname(base_dir), "server")
    zip_path = "deploy_package.zip"
    create_zip(server_dir, zip_path)

    success_urls = []
    regions = conf['deployment']['regions']

    # 4. 遍历部署 (每个区域可部署多个实例，函数名加 _N 后缀)
    base_name = conf['deployment']['function_name']
    instance_count = conf['deployment'].get('instance_count', 1)
    print(f"\n[+] 开始部署，共 {len(regions)} 个区域 x {instance_count} 个实例/区域")

    for region in regions:
        try:
            client = get_client(region, secret_id, secret_key)
        except Exception as e:
            print(f"[-] [{region}] 初始化客户端失败: {e}")
            continue

        for idx in range(1, instance_count + 1):
            func_name = base_name if instance_count == 1 else f"{base_name}_{idx}"
            print(f"\n>>> 处理: [{region}] {func_name}")
            try:
                if deploy_function(client, region, func_name, zip_path, conf, token):
                    url = enable_function_url(client, region, func_name, conf)
                    if url:
                        print(f"[+] [{region}] URL 获取成功: {url}")

                        # 5. 健康检查
                        if conf['health_check']['enable']:
                            if check_health(url, conf, token):
                                success_urls.append(url)
                            else:
                                print(f"[!] [{region}] {func_name} 部署成功但健康检查失败，暂不加入配置。")
                        else:
                            success_urls.append(url)
            except Exception as e:
                print(f"[-] [{region}] {func_name} 部署流程发生异常: {e}")

    # 6. 清理与生成配置
    if os.path.exists(zip_path):
        os.remove(zip_path)

    if success_urls:
        generate_client_config(success_urls, token)
        print("\n=== 部署完成! ===")
        print("您可以直接运行客户端开始使用: ./cloud-proxy.exe -C config.toml")
    else:
        print("\n[!] 没有可用的代理节点部署成功。")

if __name__ == "__main__":
    main()

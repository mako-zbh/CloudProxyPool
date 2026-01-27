# -*- coding: utf8 -*-
import os
import sys
import json
import zipfile
import time
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

def deploy_function(client, region, zip_path, conf):
    """部署或更新云函数"""
    func_name = conf['deployment']['function_name']
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
        print(f"[*] [{region}] 函数已存在，正在更新代码...")
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
        try:
            client.CreateFunction(req)
        except Exception as e:
            print(f"[错误] [{region}] 创建函数失败: {e}")
            return False
    
    # 等待状态同步
    time.sleep(2)
    return True

def enable_function_url(client, region, conf):
    """启用并获取函数 URL (通过创建 HTTP Trigger)"""
    func_name = conf['deployment']['function_name']
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

def check_health(url, conf):
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
        resp = requests.post(url, json=payload, timeout=10)
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

def generate_client_config(urls):
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
"""
    
    # 生成到 ../client/config.toml
    base_dir = os.path.dirname(os.path.abspath(__file__))
    client_dir = os.path.join(os.path.dirname(base_dir), "client")
    config_path = os.path.join(client_dir, "config.toml")
    
    with open(config_path, "w", encoding="utf-8") as f:
        f.write(config_content)
    print(f"\n[+] 客户端配置文件已生成: '{config_path}' (包含 {len(urls)} 个可用节点)")

def main():
    # 1. 加载配置
    print("=== Cloud ProxyPool 自动化部署工具 ===")
    conf = load_config()
    secret_id = conf['tencent']['secret_id']
    secret_key = conf['tencent']['secret_key']
    
    if "YOUR_SECRET_ID" in secret_id:
        print("[提示] 请先修改 configuration file 中的密钥信息！")
        sys.exit(1)

    # 2. 打包
    base_dir = os.path.dirname(os.path.abspath(__file__))
    server_dir = os.path.join(os.path.dirname(base_dir), "server")
    zip_path = "deploy_package.zip"
    create_zip(server_dir, zip_path)

    success_urls = []
    regions = conf['deployment']['regions']

    # 3. 遍历部署
    print(f"\n[+] 开始部署，共 {len(regions)} 个区域: {regions}")
    for region in regions:
        print(f"\n>>> 处理区域: {region}")
        try:
            client = get_client(region, secret_id, secret_key)
            if deploy_function(client, region, zip_path, conf):
                url = enable_function_url(client, region, conf)
                if url:
                    print(f"[+] [{region}] URL 获取成功: {url}")
                    
                    # 4. 健康检查
                    if conf['health_check']['enable']:
                        if check_health(url, conf):
                            success_urls.append(url)
                        else:
                            print(f"[!] [{region}] 部署成功但健康检查失败，暂不加入配置。")
                    else:
                        success_urls.append(url)
        except Exception as e:
            print(f"[-] [{region}] 部署流程发生异常: {e}")

    # 5. 清理与生成配置
    if os.path.exists(zip_path):
        os.remove(zip_path)

    if success_urls:
        generate_client_config(success_urls)
        print("\n=== 部署完成! ===")
        print("您可以直接运行客户端开始使用: ./cloud-proxy.exe -C config.toml")
    else:
        print("\n[!] 没有可用的代理节点部署成功。")

if __name__ == "__main__":
    main()

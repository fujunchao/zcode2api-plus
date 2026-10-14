"""为固定 New API 源码安装隔离合同回归；仅在 Ubuntu CI 的独立 checkout 执行。"""

from pathlib import Path
import argparse
import subprocess


PIN = "2035a82aeb5414253a728bd937d4b8f97aa99b9b"
HERE = Path(__file__).resolve().parent
ANCHOR = "\tCloseResponseBodyGracefully(resp)\n\tvar errResponse dto.GeneralErrorResponse\n"
PATCHED = "\tCloseResponseBodyGracefully(resp)\n\tdefer func() { applyZcode2apiRetryAdvice(ctx, resp, responseBody, newApiErr) }()\n\tvar errResponse dto.GeneralErrorResponse\n"
CONTROLLER_ANCHOR = "\t\tc.Request.Body = io.NopCloser(bodyStorage)\n"
CONTROLLER_PATCHED = CONTROLLER_ANCHOR + "\t\tc.Request = c.Request.WithContext(service.Zcode2apiRetryContext(c))\n"
ATTRIBUTION_ANCHOR = "\t\tclientHeaderValue := c.Request.Header.Get(name)\n"
ATTRIBUTION_PATCHED = "\t\tif value, include, handled := zcode2apiAttributionPlaceholder(c, name); handled {\n\t\t\treturn value, include, nil\n\t\t}\n" + ATTRIBUTION_ANCHOR


def git(root: Path, *args: str) -> str:
    return subprocess.check_output(["git", "-C", str(root), *args], text=True).strip()


def write_owned(root: Path, name: str, source: str) -> None:
    target = root / "service" / name
    if target.exists() and target.read_text(encoding="utf-8") != source:
        raise SystemExit(f"拒绝覆盖非本脚本生成的文件：{target}")
    target.write_text(source, encoding="utf-8", newline="\n")


def write_channel_owned(root: Path, name: str, source: str) -> None:
    target = root / "relay" / "channel" / name
    if target.exists() and target.read_text(encoding="utf-8") != source:
        raise SystemExit(f"拒绝覆盖非本脚本生成的渠道夹具：{target}")
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(source, encoding="utf-8", newline="\n")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("checkout", type=Path)
    parser.add_argument("mode", choices=("baseline", "patched"))
    args = parser.parse_args()
    root = args.checkout.resolve(strict=True)
    if git(root, "rev-parse", "HEAD") != PIN:
        raise SystemExit("拒绝应用到其他 New API 版本；需重新核查契约而非忽略版本门禁")
    changes = []
    for relative, anchor, replacement in (
        ("service/error.go", ANCHOR, PATCHED),
        ("controller/relay.go", CONTROLLER_ANCHOR, CONTROLLER_PATCHED),
        ("relay/channel/api_request.go", ATTRIBUTION_ANCHOR, ATTRIBUTION_PATCHED),
    ):
        target = root / relative
        original = subprocess.check_output(["git", "-C", str(root), "show", f"HEAD:{relative}"], text=True)
        if original.count(anchor) != 1:
            raise SystemExit(f"固定版本的锚点不唯一或已变化：{relative}")
        current = target.read_text(encoding="utf-8")
        patched = original.replace(anchor, replacement)
        if current not in (original, patched):
            raise SystemExit(f"存在无关改动，拒绝覆盖：{relative}")
        if args.mode == "baseline" and current != original:
            raise SystemExit("原版验收必须使用干净错误处理器和controller，不能把补丁版当成原版")
        changes.append((target, patched))
    write_owned(root, "zcode2api_retry_contract_test.go", (HERE / "zcode2api_retry_test.go.txt").read_text(encoding="utf-8"))
    # baseline 仅安装尚无生产调用点的兼容函数，原 controller/解析/重试逻辑完全不改。
    write_owned(root, "zcode2api_retry_contract.go", (HERE / "zcode2api_retry.go.txt").read_text(encoding="utf-8"))
    write_channel_owned(root, "zcode2api_attribution_contract_test.go", (HERE / "zcode2api_attribution_test.go.txt").read_text(encoding="utf-8"))
    write_channel_owned(root, "zcode2api_attribution_contract.go", (HERE / "zcode2api_attribution.go.txt").read_text(encoding="utf-8"))
    write_channel_owned(root, "testdata/zcode2api-attribution-headers.json", (HERE / "attribution-headers.rc41.json").read_text(encoding="utf-8"))
    if args.mode == "patched":
        for target, patched in changes:
            target.write_text(patched, encoding="utf-8", newline="\n")
    print(f"New API {PIN}: {args.mode} 合同夹具已安装，尚未执行测试")


if __name__ == "__main__":
    main()

# 签名金标向量

`pkg/hyperliquid/testdata/l1_golden.json` 与 `px_golden.json` 由官方 `hyperliquid-python-sdk==0.24.0` 生成，
测试要求 action hash 与签名逐字节一致（主网 / 测试网，含 cloid、builder、vaultAddress、expiresAfter、cancel、updateLeverage）。
私钥是 SDK 测试用的公开私钥，不对应任何资金。

重新生成（在临时目录，勿在仓库内安装依赖）：

```bash
python3 -m venv /tmp/hlsdk && /tmp/hlsdk/bin/pip install hyperliquid-python-sdk==0.24.0
/tmp/hlsdk/bin/python -I scripts/golden/gen_l1_golden.py \
  "$(/tmp/hlsdk/bin/python -c 'import site;print(site.getsitepackages()[0])')" \
  > pkg/hyperliquid/testdata/l1_golden.json
```

另外已在 testnet 用随机未注资私钥验证：venue 返回的 `User or API Wallet 0x… does not exist` 中的地址与签名地址一致，说明线上接受该签名格式。

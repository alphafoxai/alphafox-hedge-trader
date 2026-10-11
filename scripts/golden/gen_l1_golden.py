import json, sys
sys.path.insert(0, sys.argv[1])
from eth_account import Account
from hyperliquid.utils.signing import sign_l1_action, action_hash, order_request_to_order_wire, order_wires_to_order_action
from hyperliquid.utils.types import Cloid
key = "0x0123456789012345678901234567890123456789012345678901234567890123"
w = Account.from_key(key)
nonce = 1760000000000
cases = []
o1 = order_request_to_order_wire({"coin":"BTC","is_buy":True,"sz":0.0123,"limit_px":62345.0,"reduce_only":False,"order_type":{"limit":{"tif":"Ioc"}}}, 0)
cases.append(("order_ioc", order_wires_to_order_action([o1]), None, None))
o2 = order_request_to_order_wire({"coin":"ETH","is_buy":False,"sz":1.5,"limit_px":2400.5,"reduce_only":True,"order_type":{"limit":{"tif":"Gtc"}},"cloid":Cloid.from_str("0x00000000000000000000000000000001")}, 1)
cases.append(("order_cloid_builder", order_wires_to_order_action([o2], {"b":"0xf945417968b53adc1e0489e5a287d2e41c8b3d79","f":50}), None, None))
cases.append(("cancel", {"type":"cancel","cancels":[{"a":0,"o":123456789}]}, None, None))
cases.append(("update_leverage", {"type":"updateLeverage","asset":1,"isCross":False,"leverage":20}, None, None))
cases.append(("order_vault_expires", order_wires_to_order_action([o1]), "0x1719884eb866cb12b2287399b15f7db5e7d775ea", 1760000060000))
out = []
for name, action, vault, exp in cases:
    for mainnet in (True, False):
        sig = sign_l1_action(w, action, vault, nonce, exp, mainnet)
        out.append({"name": name, "mainnet": mainnet, "action": action, "vaultAddress": vault, "expiresAfter": exp, "nonce": nonce,
                    "actionHash": "0x"+action_hash(action, vault, nonce, exp).hex(), "signature": sig})
print(json.dumps({"privateKey": key, "address": w.address, "cases": out}, indent=1))

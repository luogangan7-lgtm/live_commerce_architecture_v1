// Buyer payment copy. Consumed by components/OrderPayment.tsx (no BFF route of its own).
// Stripe keys (CLOSED_UNPAID, cancel*, creating, cutoff): contracts/stripe-buyer-ui-v1.md §7.
import type { Locale } from "@live-commerce/i18n";

const en = {
  title: "Payment",
  paymentState: "Payment status",
  orderState: "Order status",
  pay: "Pay in a new tab",
  recover: "Continue original payment",
  loading: "Checking payment availability…",
  sending: "Opening the secure payment page… Keep this store tab open.",
  explain: "Payment opens in a separate tab. Return here to check the result.",
  unavailable:
    "No payment method is available for this order. Creating an order does not take payment.",
  failed:
    "Payment status could not be confirmed. Refresh this order to check again; do not place it again.",
  blocked:
    "The payment tab could not be opened. Allow pop-ups for this store, then try again.",
  uncertain:
    "The payment page could not be confirmed. Refresh this order to check its status. A payment page that was already issued will not be issued again.",
  submitted:
    "Payment handoff requested. Check the other tab, then refresh this order for the result. This does not confirm payment.",
  readOnly:
    "Continue in the payment tab already opened. If it is unavailable, refresh this order or contact the store; do not pay again.",
  test: "Test payment — not a live payment",
  NOT_STARTED: "Not paid",
  PENDING: "Waiting for the payment result",
  AUTHORIZED: "Authorized — capture not yet confirmed",
  CAPTURED: "Payment capture recorded",
  PARTIALLY_REFUNDED: "Partially refunded",
  REFUNDED: "Refunded",
  REVIEW_REQUIRED: "Payment needs review — do not pay again",
  CLOSED_UNPAID: "Payment closed without charge",
  refundProcessing: "Refund processing",
  // {amount} = the succeeded refund total (stripe-refund-v1 §7.2, F-R8); pending money is never shown as refunded.
  refunded:
    "Refunded {amount} — your bank may take 5–10 business days",
  cancel: "Cancel payment",
  cancelConfirm:
    "Cancel this payment? The order is cancelled once the payment provider confirms. If you already paid, the payment stands.",
  cancelling:
    "Cancellation requested. Waiting for the payment provider to confirm; refresh this order to check.",
  creating:
    "The secure payment page is still being prepared. Try Continue original payment again shortly.",
  cutoff:
    "This payment page can no longer be opened. Refresh this order for the result.",
};
type Copy = { [K in keyof typeof en]: string };
export const paymentCopy: Record<Locale, Copy> = {
  en,
  "zh-CN": {
    title: "付款",
    paymentState: "付款状态",
    orderState: "订单状态",
    pay: "前往新标签页付款",
    recover: "继续原付款请求",
    loading: "正在确认付款方式…",
    sending: "正在打开安全付款页…请保留此商店标签页。",
    explain: "付款会在新标签页打开，完成后回到这里查询结果。",
    unavailable: "此订单暂时没有可用的付款方式。创建订单不会扣款。",
    failed: "暂时无法确认付款状态，请刷新此订单查询，不要重复下单。",
    blocked: "未能打开付款标签页，请允许此商店弹出窗口后重试。",
    uncertain:
      "尚未确认付款页是否打开，请刷新此订单查询状态。已经发出的付款页不会重复发出。",
    submitted:
      "已请求跳转付款，请查看另一个标签页，再刷新此订单查询结果。这不代表付款成功。",
    readOnly:
      "请在已打开的付款标签页继续。如无法使用，请刷新此订单或联系商家，不要重复付款。",
    test: "测试付款，不是真实付款",
    NOT_STARTED: "尚未付款",
    PENDING: "等待付款结果",
    AUTHORIZED: "已授权，尚未确认请款",
    CAPTURED: "已记录付款请款结果",
    PARTIALLY_REFUNDED: "已部分退款",
    REFUNDED: "已退款",
    REVIEW_REQUIRED: "付款需要核查，请勿重复付款",
    CLOSED_UNPAID: "付款已关闭，未扣款",
    refundProcessing: "退款处理中",
    refunded: "已退款 {amount}，银行入账可能需要 5–10 个工作日",
    cancel: "取消付款",
    cancelConfirm:
      "确定取消付款？支付服务商确认后订单会取消。如已付款，以付款为准。",
    cancelling: "已请求取消，正在等待支付服务商确认，请刷新此订单查询。",
    creating: "安全付款页仍在准备中，请稍后再点击“继续原付款请求”。",
    cutoff: "此付款页已无法再打开，请刷新此订单查询结果。",
  },
  "zh-TW": {
    title: "付款",
    paymentState: "付款狀態",
    orderState: "訂單狀態",
    pay: "前往新分頁付款",
    recover: "繼續原付款請求",
    loading: "正在確認付款方式…",
    sending: "正在開啟安全付款頁…請保留此商店分頁。",
    explain: "付款會在新分頁開啟，完成後回到這裡查詢結果。",
    unavailable: "此訂單暫時沒有可用的付款方式。建立訂單不會扣款。",
    failed: "暫時無法確認付款狀態，請重新整理此訂單查詢，不要重複下單。",
    blocked: "無法開啟付款分頁，請允許此商店的彈出式視窗後重試。",
    uncertain:
      "尚未確認付款頁是否開啟，請重新整理此訂單查詢狀態。已發出的付款頁不會重複發出。",
    submitted:
      "已請求跳轉付款，請查看另一個分頁，再重新整理此訂單查詢結果。這不代表付款成功。",
    readOnly:
      "請在已開啟的付款分頁繼續。如無法使用，請重新整理此訂單或聯絡商家，不要重複付款。",
    test: "測試付款，不是真實付款",
    NOT_STARTED: "尚未付款",
    PENDING: "等待付款結果",
    AUTHORIZED: "已授權，尚未確認請款",
    CAPTURED: "已記錄付款請款結果",
    PARTIALLY_REFUNDED: "已部分退款",
    REFUNDED: "已退款",
    REVIEW_REQUIRED: "付款需要核查，請勿重複付款",
    CLOSED_UNPAID: "付款已關閉，未扣款",
    refundProcessing: "退款處理中",
    refunded: "已退款 {amount}，銀行入帳可能需要 5–10 個工作天",
    cancel: "取消付款",
    cancelConfirm:
      "確定取消付款？支付服務商確認後訂單會取消。如已付款，以付款為準。",
    cancelling: "已請求取消，正在等待支付服務商確認，請重新整理此訂單查詢。",
    creating: "安全付款頁仍在準備中，請稍後再點選「繼續原付款請求」。",
    cutoff: "此付款頁已無法再開啟，請重新整理此訂單查詢結果。",
  },
};

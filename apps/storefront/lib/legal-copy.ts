// Owns the static storefront legal copy (privacy, terms, refunds/returns/cancellation/disputes,
// shipping, contact) in zh-TW, zh-CN and en, and the footer link list.
// Contract: stripe-live-enable-v1 §1 L9/§9 policy_pages/§14 LQ7, customers-billing-v1 F-P2/CD7,
// meta-ads-v1 App Review URLs; docs/delivery/units/legal-pages.md (P1-P3).
// Calls no BFF / no Go endpoint / no SQL: pure data, imported by the legal page and LegalFooter.
// Non-goals: engineering never invents policy terms. Every commitment is `owner(...)` (a value the
// owner must supply) or `draft(...)` (derived from contracts, awaiting owner approval); `final` text
// exists only after the owner pastes/approves it. No consent capture, no deletion execution, no
// per-store text (W1: one platform-level set for the owner's own store, 香港大碗貿易有限公司).
import type { Locale } from "@live-commerce/i18n";

export const legalSlugs = ["privacy", "terms", "refunds", "shipping", "contact"] as const;
export type LegalSlug = (typeof legalSlugs)[number];
export type LegalText = { kind: "final" | "draft" | "pending"; text: string };
type Section = { heading: string; body: LegalText[] };
type Page = { title: string; updated: LegalText; sections: Section[] };

const owner = (what: string): LegalText => ({ kind: "pending", text: what });
const draft = (text: string): LegalText => ({ kind: "draft", text });
const sec = (heading: string, ...body: LegalText[]): Section => ({ heading, body });

// Marker labels are bilingual in every locale so a reviewer of any language sees the same warning.
export const markerLabel = {
  pending: "待業主提供 · Owner text pending:",
  draft: "草稿 · Draft pending owner approval",
} as const;

const chrome: Record<Locale, { updated: string; nav: string; deletion: string }> = {
  "zh-TW": { updated: "最後更新", nav: "政策與法律資訊", deletion: "資料刪除" },
  "zh-CN": { updated: "最后更新", nav: "政策与法律信息", deletion: "数据删除" },
  en: { updated: "Last updated", nav: "Policies and legal", deletion: "Data deletion" },
};
export const legalChrome = (locale: Locale) => chrome[locale];

// ---- English -------------------------------------------------------------------------------
const en = (l: Locale): Record<LegalSlug, Page> => ({
  privacy: {
    title: "Privacy Policy",
    updated: owner("Policy effective date"),
    sections: [
      sec("Who we are",
        owner("Legal entity name (proposed: 香港大碗貿易有限公司; confirm)"),
        owner("Registered business address"),
        owner("Privacy contact email")),
      sec("What we collect",
        draft("Order details: the items, quantities, prices and currency of your order, and its payment and delivery status."),
        draft("Delivery details you enter at checkout: recipient name, phone number, and a delivery address or convenience-store pickup point."),
        draft("If you comment on one of our live-stream posts on Facebook or Instagram, Meta gives us your comment and an app-scoped identifier so we can send you your cart link by private message. We do not receive your Meta password."),
        draft("Card details are entered on Stripe's payment page. We never receive or store your full card number; we keep Stripe's payment reference and status."),
        draft("A shopping-session identifier is kept in your browser so you can return to your cart and orders. It is strictly necessary; we set no advertising cookies today.")),
      sec("How we use it",
        draft("To take payment, fulfil and ship your order, show you its status, handle refunds and support requests, and detect fraud or abuse."),
        owner("Any marketing use of contact details, and the consent wording for it"),
        owner("Retention period for order, payment and delivery records")),
      sec("Who we share it with",
        draft("Stripe (payment processing), the delivery carrier (recipient name, phone and address), and Meta (private messages on its platform)."),
        owner("Any other processors or hosting locations to be named")),
      sec("Your data and deletion",
        draft(`You can ask us to delete your profile, contact and delivery details that are not tied to an order, and to withdraw marketing consent. Instructions: /${l}/data-deletion. Shoppers with an active shopping session can also use the self-service page at /${l}/privacy.`),
        draft("Order, payment, refund and shipment records are kept for legal and financial reasons and are not removed by a deletion request.")),
      sec("Governing law", owner("Governing law and complaint route for privacy matters")),
    ],
  },
  terms: {
    title: "Terms of Service",
    updated: owner("Terms effective date"),
    sections: [
      sec("About these terms",
        owner("Legal entity name that sells and operates this store"),
        owner("Business description: the goods sold"),
        draft("By placing an order you agree to these terms and to the shipping, refund and privacy pages.")),
      sec("Ordering and prices",
        draft("Prices are shown on each product page with their currency. An order is confirmed only after payment is confirmed."),
        draft("Stock is held for you only for a short time while you pay; if payment does not complete, the hold is released."),
        draft("Card payments are processed by Stripe.")),
      sec("Delivery and returns", draft("Delivery, refunds, returns, cancellation and disputes are set out on the Shipping and Refunds pages.")),
      sec("Liability and governing law",
        owner("Limitation of liability wording"),
        owner("Governing law and jurisdiction")),
      sec("Changes", owner("How changes to these terms are announced")),
    ],
  },
  refunds: {
    title: "Refunds, Returns and Cancellation",
    updated: owner("Policy effective date"),
    sections: [
      sec("Refunds",
        owner("Refund window (days from delivery or payment)"),
        draft("Approved refunds are returned to the original payment method through Stripe. The time it takes to appear depends on your card issuer."),
        owner("Refund processing time we promise")),
      sec("Returns",
        owner("Return conditions (condition of goods, exclusions, proof required)"),
        owner("Who pays return shipping")),
      sec("Cancellation", owner("Cancellation rule (until when an order can be cancelled, and how to ask)")),
      sec("Disputes and chargebacks",
        draft("If something is wrong with your order, contact us first so we can fix it. You may also dispute a card payment with your card issuer; the dispute is then handled through Stripe, whose response window is usually 7 to 21 days."),
        owner("Support email for refund and dispute requests")),
    ],
  },
  shipping: {
    title: "Shipping",
    updated: owner("Policy effective date"),
    sections: [
      sec("Where we ship",
        owner("Shipping regions"),
        draft("Parcels ship from mainland China to Taiwan through cross-border logistics, to a convenience-store pickup point or an address you enter at checkout.")),
      sec("Carriers and lead times",
        owner("Carriers used"),
        owner("Lead time from payment to dispatch, and transit time")),
      sec("Fees and import charges", owner("Shipping fees, free-shipping threshold, and who pays import taxes or duties")),
      sec("Tracking", draft("When the carrier and tracking number are recorded for your order, you can see them on your order page.")),
    ],
  },
  contact: {
    title: "Contact",
    updated: owner("Policy effective date"),
    sections: [
      sec("Operator",
        owner("Legal entity name (proposed: 香港大碗貿易有限公司; confirm)"),
        owner("Business description: the goods sold")),
      sec("How to reach us",
        owner("Registered business address"),
        owner("Customer support email"),
        owner("Customer support phone")),
      sec("Response times", owner("Support hours and target response time")),
      sec("Data requests", draft(`To delete your data, see /${l}/data-deletion.`)),
    ],
  },
});

// ---- 繁體中文 ------------------------------------------------------------------------------
const zhTW = (l: Locale): Record<LegalSlug, Page> => ({
  privacy: {
    title: "隱私權政策",
    updated: owner("政策生效日期"),
    sections: [
      sec("我們是誰",
        owner("法律主體名稱（建議：香港大碗貿易有限公司，待確認）"),
        owner("登記營業地址"),
        owner("隱私事務聯絡電子郵件")),
      sec("我們蒐集什麼",
        draft("訂單資料：訂單的商品、數量、價格與幣別，以及付款與出貨狀態。"),
        draft("您在結帳時填寫的收件資料：收件人姓名、電話，以及收件地址或超商取貨門市。"),
        draft("若您在我們於 Facebook 或 Instagram 的直播貼文留言，Meta 會將您的留言與應用程式範圍識別碼提供給我們，以便以私訊寄送購物車連結。我們不會取得您的 Meta 密碼。"),
        draft("信用卡資料由您在 Stripe 的付款頁面輸入。我們不會收到或儲存完整卡號，只保留 Stripe 的付款編號與狀態。"),
        draft("瀏覽器會保存一個購物工作階段識別碼，讓您能回到購物車與訂單。它屬必要功能；目前我們不設定廣告 Cookie。")),
      sec("我們如何使用",
        draft("用於收款、處理與寄出訂單、顯示訂單狀態、處理退款與客服需求，以及偵測詐欺或濫用。"),
        owner("是否將聯絡資料用於行銷，以及取得同意的文字"),
        owner("訂單、付款與收件資料的保存期間")),
      sec("我們與誰分享",
        draft("Stripe（付款處理）、物流業者（收件人姓名、電話與地址），以及 Meta（於其平台上的私訊）。"),
        owner("需列明的其他處理者或主機所在地")),
      sec("您的資料與刪除",
        draft(`您可以要求刪除未與訂單綁定的個人檔案、聯絡與收件資料，並撤回行銷同意。說明請見 /${l}/data-deletion。已取得購物工作階段的買家也可使用 /${l}/privacy 的自助頁面。`),
        draft("訂單、付款、退款與出貨紀錄基於法律與財務保存要求而保留，不會因刪除要求而移除。")),
      sec("準據法", owner("隱私事務的準據法與申訴管道")),
    ],
  },
  terms: {
    title: "服務條款",
    updated: owner("條款生效日期"),
    sections: [
      sec("關於本條款",
        owner("銷售並營運本商店的法律主體名稱"),
        owner("營業項目說明：所售商品"),
        draft("下單即表示您同意本條款，以及運送、退款與隱私權頁面的內容。")),
      sec("下單與價格",
        draft("各商品頁面標示價格與幣別。付款確認後訂單才成立。"),
        draft("付款期間我們只會短時間保留庫存；若付款未完成，保留即釋放。"),
        draft("信用卡付款由 Stripe 處理。")),
      sec("運送與退貨", draft("運送、退款、退貨、取消與爭議請見「運送」與「退款與退貨」頁面。")),
      sec("責任與準據法",
        owner("責任限制條款文字"),
        owner("準據法與管轄法院")),
      sec("條款變更", owner("條款變更的公告方式")),
    ],
  },
  refunds: {
    title: "退款、退貨與取消",
    updated: owner("政策生效日期"),
    sections: [
      sec("退款",
        owner("退款期限（自收貨或付款起算天數）"),
        draft("核准的退款會透過 Stripe 退回原付款方式。實際入帳時間取決於發卡銀行。"),
        owner("承諾的退款處理時間")),
      sec("退貨",
        owner("退貨條件（商品狀態、排除項目、所需證明）"),
        owner("退貨運費由誰負擔")),
      sec("取消", owner("取消規則（訂單可取消至何時、如何申請）")),
      sec("爭議與信用卡拒付",
        draft("若訂單有問題，請先聯絡我們處理。您也可以向發卡銀行對該筆信用卡付款提出爭議；爭議將透過 Stripe 處理，其回應期限通常為 7 至 21 天。"),
        owner("退款與爭議申請的客服電子郵件")),
    ],
  },
  shipping: {
    title: "運送",
    updated: owner("政策生效日期"),
    sections: [
      sec("配送範圍",
        owner("配送地區"),
        draft("包裹由中國大陸經跨境物流寄往台灣，送至超商取貨門市或您於結帳時填寫的地址。")),
      sec("物流業者與時程",
        owner("使用的物流業者"),
        owner("付款至出貨的時程與運送時間")),
      sec("費用與進口稅費", owner("運費、免運門檻，以及進口稅費由誰負擔")),
      sec("追蹤", draft("當訂單記錄了物流業者與追蹤編號後，您可在訂單頁面查看。")),
    ],
  },
  contact: {
    title: "聯絡我們",
    updated: owner("政策生效日期"),
    sections: [
      sec("營運主體",
        owner("法律主體名稱（建議：香港大碗貿易有限公司，待確認）"),
        owner("營業項目說明：所售商品")),
      sec("聯絡方式",
        owner("登記營業地址"),
        owner("客服電子郵件"),
        owner("客服電話")),
      sec("回覆時間", owner("客服時間與目標回覆時間")),
      sec("資料相關要求", draft(`如需刪除您的資料，請見 /${l}/data-deletion。`)),
    ],
  },
});

// ---- 简体中文 ------------------------------------------------------------------------------
const zhCN = (l: Locale): Record<LegalSlug, Page> => ({
  privacy: {
    title: "隐私政策",
    updated: owner("政策生效日期"),
    sections: [
      sec("我们是谁",
        owner("法律主体名称（建议：香港大碗貿易有限公司，待确认）"),
        owner("注册营业地址"),
        owner("隐私事务联系邮箱")),
      sec("我们收集什么",
        draft("订单信息：订单的商品、数量、价格与币种，以及付款与发货状态。"),
        draft("您在结账时填写的收件信息：收件人姓名、电话，以及收件地址或便利店取货门店。"),
        draft("如果您在我们于 Facebook 或 Instagram 的直播帖子下留言，Meta 会把您的留言与应用范围标识符提供给我们，以便通过私信发送购物车链接。我们不会获取您的 Meta 密码。"),
        draft("银行卡信息由您在 Stripe 的支付页面输入。我们不会接收或存储完整卡号，只保留 Stripe 的支付编号与状态。"),
        draft("浏览器会保存一个购物会话标识符，使您可以回到购物车与订单。它属于必要功能；目前我们不设置广告 Cookie。")),
      sec("我们如何使用",
        draft("用于收款、处理并寄出订单、显示订单状态、处理退款与客服请求，以及识别欺诈或滥用。"),
        owner("是否将联系信息用于营销，以及获取同意的文字"),
        owner("订单、付款与收件信息的保存期限")),
      sec("我们与谁共享",
        draft("Stripe（支付处理）、物流承运商（收件人姓名、电话与地址），以及 Meta（其平台上的私信）。"),
        owner("需列明的其他处理方或服务器所在地")),
      sec("您的数据与删除",
        draft(`您可以要求删除未与订单绑定的个人档案、联系与收件信息，并撤回营销同意。说明见 /${l}/data-deletion。已获得购物会话的买家也可使用 /${l}/privacy 的自助页面。`),
        draft("订单、付款、退款与发货记录基于法律与财务保存要求而保留，不会因删除请求而移除。")),
      sec("适用法律", owner("隐私事务的适用法律与投诉渠道")),
    ],
  },
  terms: {
    title: "服务条款",
    updated: owner("条款生效日期"),
    sections: [
      sec("关于本条款",
        owner("销售并运营本店铺的法律主体名称"),
        owner("经营范围说明：所售商品"),
        draft("下单即表示您同意本条款，以及运送、退款与隐私页面的内容。")),
      sec("下单与价格",
        draft("各商品页面标示价格与币种。付款确认后订单才成立。"),
        draft("付款期间我们只会短时间保留库存；如果付款未完成，保留将被释放。"),
        draft("银行卡付款由 Stripe 处理。")),
      sec("运送与退货", draft("运送、退款、退货、取消与争议见“运送”与“退款与退货”页面。")),
      sec("责任与适用法律",
        owner("责任限制条款文字"),
        owner("适用法律与管辖法院")),
      sec("条款变更", owner("条款变更的公告方式")),
    ],
  },
  refunds: {
    title: "退款、退货与取消",
    updated: owner("政策生效日期"),
    sections: [
      sec("退款",
        owner("退款期限（自收货或付款起算天数）"),
        draft("获批的退款会通过 Stripe 退回原支付方式。实际到账时间取决于发卡银行。"),
        owner("承诺的退款处理时间")),
      sec("退货",
        owner("退货条件（商品状态、排除项目、所需证明）"),
        owner("退货运费由谁承担")),
      sec("取消", owner("取消规则（订单可取消至何时、如何申请）")),
      sec("争议与拒付",
        draft("如果订单有问题，请先联系我们处理。您也可以向发卡银行对该笔银行卡付款提出争议；争议将通过 Stripe 处理，其响应期限通常为 7 至 21 天。"),
        owner("退款与争议申请的客服邮箱")),
    ],
  },
  shipping: {
    title: "运送",
    updated: owner("政策生效日期"),
    sections: [
      sec("配送范围",
        owner("配送地区"),
        draft("包裹由中国大陆经跨境物流寄往台湾，送至便利店取货门店或您在结账时填写的地址。")),
      sec("承运商与时效",
        owner("使用的承运商"),
        owner("付款至发货的时效与运输时间")),
      sec("费用与进口税费", owner("运费、包邮门槛，以及进口税费由谁承担")),
      sec("物流跟踪", draft("订单记录了承运商与运单号后，您可以在订单页面查看。")),
    ],
  },
  contact: {
    title: "联系我们",
    updated: owner("政策生效日期"),
    sections: [
      sec("运营主体",
        owner("法律主体名称（建议：香港大碗貿易有限公司，待确认）"),
        owner("经营范围说明：所售商品")),
      sec("联系方式",
        owner("注册营业地址"),
        owner("客服邮箱"),
        owner("客服电话")),
      sec("响应时间", owner("客服时间与目标响应时间")),
      sec("数据相关请求", draft(`如需删除您的数据，请见 /${l}/data-deletion。`)),
    ],
  },
});

const pages: Record<Locale, (l: Locale) => Record<LegalSlug, Page>> = { en, "zh-TW": zhTW, "zh-CN": zhCN };

export function legalPage(locale: Locale, slug: LegalSlug): Page {
  return pages[locale](locale)[slug];
}

export function legalFooterLinks(locale: Locale): { href: string; label: string }[] {
  return [
    ...legalSlugs.map((slug) => ({ href: `/${locale}/legal/${slug}`, label: legalPage(locale, slug).title })),
    { href: `/${locale}/data-deletion`, label: chrome[locale].deletion },
  ];
}

// The owner's to-do list: exactly the non-final texts legalPage() makes the page render as markers.
export function pendingOwnerText(): { locale: Locale; page: string; what: string; kind: "draft" | "pending" }[] {
  const out: { locale: Locale; page: string; what: string; kind: "draft" | "pending" }[] = [];
  for (const locale of Object.keys(pages) as Locale[]) {
    for (const slug of legalSlugs) {
      const p = legalPage(locale, slug);
      for (const t of [p.updated, ...p.sections.flatMap((s) => s.body)]) {
        if (t.kind !== "final") out.push({ locale, page: slug, what: t.text, kind: t.kind });
      }
    }
  }
  return out;
}

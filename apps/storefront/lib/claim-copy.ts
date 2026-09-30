// Owns every visible string of the buyer claim page (/{locale}/claim) in zh-CN, zh-TW
// and en, including the contract's 404 and 409 copy (live-keyword-claims-v1 §11.1).
// Non-goals: no money/time formatting (the page uses Intl), no claim rule.
// Depends on: @live-commerce/i18n (Locale) only.

import type { Locale } from "@live-commerce/i18n";

const en = {
  store: "Storefront", language: "Language", demonstration: "Synthetic data · test environment",
  title: "Your claimed items", loading: "Opening your claim…",
  intro: "These items were recorded from your live comments. Review them, then add them to your cart.",
  price: "Current price, final at checkout", stock: "Claims do not reserve stock. Stock is confirmed at checkout.",
  quantity: "Quantity", keyword: "Code", inCart: "Already in your cart", unavailable: "Not available right now",
  add: "Add to cart", adding: "Adding…", addAgain: "Update my cart",
  expires: (time: string) => `This link works until ${time}.`,
  added: "Added to your cart.", nothing: "Your cart already has these items.",
  skipped: "Some items were not added because they are not available right now. They stay on this link.",
  notFound: "This link expired or was replaced — ask the seller for a new link",
  conflict: "Your cart has an item that is no longer available, or the claim changed. Review your cart, then try again.",
  reviewCart: "Review your cart", reload: "Reload claim",
  failed: "We could not confirm the result. Reload the claim before trying again.",
  session: "This shopping session needs to be renewed.", renew: "Renew shopping session",
  cart: "Your cart", emptyCart: "Your cart is empty.", otherItem: "Other item",
  remove: "Remove", cartFailed: "Your cart could not be updated. Reload the claim and try again.",
};
export type ClaimCopy = typeof en;

export const claimCopy: Record<Locale, ClaimCopy> = {
  en,
  "zh-CN": {
    store: "店铺前台", language: "语言", demonstration: "合成数据 · 测试环境",
    title: "你登记的商品", loading: "正在打开登记…",
    intro: "这些商品来自你在直播中的评论。请确认后加入购物车。",
    price: "当前价格，以结账时为准", stock: "登记不会保留库存，结账时才确认库存。",
    quantity: "数量", keyword: "口令", inCart: "已在购物车中", unavailable: "目前无法购买",
    add: "加入购物车", adding: "正在加入…", addAgain: "更新我的购物车",
    expires: (time: string) => `此链接有效至 ${time}。`,
    added: "已加入购物车。", nothing: "购物车里已有这些商品。",
    skipped: "部分商品目前无法购买，未加入购物车，仍保留在此链接中。",
    notFound: "此链接已过期或已被替换——请向卖家索取新链接",
    conflict: "你的购物车中有商品已无法购买，或登记内容已变更。请先检查购物车，然后再试一次。",
    reviewCart: "检查购物车", reload: "重新读取登记",
    failed: "无法确认结果。请先重新读取登记，再试一次。",
    session: "此购物会话需要更新。", renew: "更新购物会话",
    cart: "你的购物车", emptyCart: "购物车是空的。", otherItem: "其他商品",
    remove: "移除", cartFailed: "无法更新购物车。请重新读取登记后再试。",
  },
  "zh-TW": {
    store: "商店前台", language: "語言", demonstration: "合成資料 · 測試環境",
    title: "你登記的商品", loading: "正在開啟登記…",
    intro: "這些商品來自你在直播中的留言。請確認後加入購物車。",
    price: "目前價格，以結帳時為準", stock: "登記不會保留庫存，結帳時才確認庫存。",
    quantity: "數量", keyword: "關鍵字", inCart: "已在購物車中", unavailable: "目前無法購買",
    add: "加入購物車", adding: "正在加入…", addAgain: "更新我的購物車",
    expires: (time: string) => `此連結有效至 ${time}。`,
    added: "已加入購物車。", nothing: "購物車裡已有這些商品。",
    skipped: "部分商品目前無法購買，未加入購物車，仍保留在此連結中。",
    notFound: "此連結已過期或已被替換——請向賣家索取新連結",
    conflict: "你的購物車中有商品已無法購買，或登記內容已變更。請先檢查購物車，然後再試一次。",
    reviewCart: "檢查購物車", reload: "重新讀取登記",
    failed: "無法確認結果。請先重新讀取登記，再試一次。",
    session: "此購物工作階段需要更新。", renew: "更新購物工作階段",
    cart: "你的購物車", emptyCart: "購物車是空的。", otherItem: "其他商品",
    remove: "移除", cartFailed: "無法更新購物車。請重新讀取登記後再試。",
  },
};

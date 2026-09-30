"use client";

// Client half of the CVS label print tab (taiwan-cvs-logistics-v1 §8, U6). BFF: POST
// /api/stores/{store}/orders/{order}/cvs-shipment/print-form {thermal} -> Go internal/httpapi/cvs.go, which answers
// a signed ECPay form {action, fields}. The form is rendered as a real <form method=post> and submitted with
// form.requestSubmit() (no inline script, so the CSP holds); a visible button is the fallback when the browser
// blocks the automatic submit. Nothing is stored: the fields exist only in this component's memory.
import { useEffect, useRef, useState } from "react";
import type { Locale } from "@live-commerce/i18n";
import { postPrintForm } from "@/lib/logistics-client";
import type { PrintForm } from "@/lib/logistics-model";
import { logisticsCopy } from "@/lib/logistics-copy";
import "@/components/settings.css";

export function CvsPrint({
  locale,
  store,
  order,
  thermal,
}: {
  locale: Locale;
  store: string;
  order: string;
  thermal: boolean;
}) {
  const lc = logisticsCopy[locale];
  const [form, setForm] = useState<PrintForm | null>(null);
  const [failure, setFailure] = useState<"" | "bad" | "signed-out" | "failed">(store && order ? "" : "bad");
  const element = useRef<HTMLFormElement>(null);
  const started = useRef(false);

  useEffect(() => {
    if (!store || !order || started.current) return;
    started.current = true; // React strict mode runs effects twice; one print-form call per tab
    void postPrintForm(store, order, thermal).then((result) => {
      if (result.ok) setForm(result.form);
      else setFailure(result.code === "unauthorized" ? "signed-out" : "failed");
    });
  }, [store, order, thermal]);

  useEffect(() => {
    if (form) element.current?.requestSubmit();
  }, [form]);

  return (
    <main className="settings-page" data-testid="cvs-print-page">
      <h1>{lc.printTabTitle}</h1>
      {failure === "" && !form && <p role="status">{lc.printTabOpening}</p>}
      {failure === "bad" && <p role="alert">{lc.printTabBad}</p>}
      {failure === "signed-out" && <p role="alert">{lc.printTabSignedOut}</p>}
      {failure === "failed" && <p role="alert">{lc.printTabFailed}</p>}
      {form && (
        <form ref={element} method="post" action={form.action} data-testid="cvs-print-form">
          {Object.entries(form.fields).map(([name, value]) => (
            <input key={name} type="hidden" name={name} value={value} />
          ))}
          <p role="status">{lc.printTabOpening}</p>
          <p className="settings-note">{lc.printTabManual}</p>
          <button type="submit" className="primary">{lc.printTabOpen}</button>
        </form>
      )}
    </main>
  );
}

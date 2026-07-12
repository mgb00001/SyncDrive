// Minimal shadcn-style primitives (kept dependency-free).
import { ButtonHTMLAttributes, HTMLAttributes, ReactNode } from "react";

function cx(...parts: (string | false | undefined)[]) {
  return parts.filter(Boolean).join(" ");
}

export function Button({
  variant = "default",
  className,
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: "default" | "outline" | "destructive" | "ghost" }) {
  const variants = {
    default: "bg-blue-600 text-white hover:bg-blue-700",
    outline: "border border-zinc-300 dark:border-zinc-700 hover:bg-zinc-100 dark:hover:bg-zinc-800",
    destructive: "bg-red-600 text-white hover:bg-red-700",
    ghost: "hover:bg-zinc-100 dark:hover:bg-zinc-800",
  } as const;
  return (
    <button
      className={cx(
        "inline-flex items-center gap-1.5 rounded-md px-3 py-1.5 text-sm font-medium transition-colors disabled:opacity-50 disabled:pointer-events-none",
        variants[variant],
        className,
      )}
      {...props}
    />
  );
}

export function Card({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      className={cx(
        "rounded-xl border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 shadow-sm",
        className,
      )}
      {...props}
    />
  );
}

export function CardHeader({ title, action }: { title: string; action?: ReactNode }) {
  return (
    <div className="flex items-center justify-between border-b border-zinc-200 dark:border-zinc-800 px-4 py-3">
      <h2 className="text-sm font-semibold">{title}</h2>
      {action}
    </div>
  );
}

export function Badge({
  tone = "neutral",
  children,
}: {
  tone?: "neutral" | "green" | "amber" | "red";
  children: ReactNode;
}) {
  const tones = {
    neutral: "bg-zinc-100 text-zinc-700 dark:bg-zinc-800 dark:text-zinc-300",
    green: "bg-emerald-100 text-emerald-800 dark:bg-emerald-900/50 dark:text-emerald-300",
    amber: "bg-amber-100 text-amber-800 dark:bg-amber-900/50 dark:text-amber-300",
    red: "bg-red-100 text-red-800 dark:bg-red-900/50 dark:text-red-300",
  } as const;
  return (
    <span className={cx("inline-flex rounded-full px-2 py-0.5 text-xs font-medium", tones[tone])}>{children}</span>
  );
}

export function Input(props: React.InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      {...props}
      className={cx(
        "w-full rounded-md border border-zinc-300 dark:border-zinc-700 bg-transparent px-3 py-1.5 text-sm outline-none focus:ring-2 focus:ring-blue-500",
        props.className,
      )}
    />
  );
}

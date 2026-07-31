import type { Metadata } from "next";
import { Geist, Geist_Mono } from "next/font/google";
import "@xterm/xterm/css/xterm.css";
import "./globals.css";

const geistSans = Geist({
  variable: "--font-geist-sans",
  subsets: ["latin"],
});

const geistMono = Geist_Mono({
  variable: "--font-geist-mono",
  subsets: ["latin"],
});

export const metadata: Metadata = {
  metadataBase: new URL("https://tinyrange.github.io/vmsh/"),
  title: "vmsh — Virtual machines that belong in your shell",
  description:
    "Download NeurodeskAppX, SquadVM, and vmsh for macOS, Windows, and Linux.",
  icons: {
    icon: "./favicon.svg",
    shortcut: "./favicon.svg",
  },
  openGraph: {
    type: "website",
    url: "https://tinyrange.github.io/vmsh/",
    title: "vmsh — Your lab, ready to run.",
    description:
      "Download NeurodeskAppX, SquadVM, and vmsh for macOS, Windows, and Linux.",
    images: [
      {
        url: "./og.png",
        width: 1760,
        height: 920,
        alt: "vmsh — Your lab, ready to run.",
      },
    ],
  },
  twitter: {
    card: "summary_large_image",
    title: "vmsh — Your lab, ready to run.",
    description:
      "Download NeurodeskAppX, SquadVM, and vmsh for macOS, Windows, and Linux.",
    images: ["./og.png"],
  },
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html lang="en">
      <body
        className={`${geistSans.variable} ${geistMono.variable} antialiased`}
      >
        {children}
      </body>
    </html>
  );
}

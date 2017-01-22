import { css } from "glamor";
import * as React from "react";

import { Loader } from "sourcegraph/components/Loader";
import { colors, typography, whitespace } from "sourcegraph/components/utils";

export function Button(props: {
	block?: boolean,
	outline?: boolean,
	size?: "tiny" | "small" | "large",
	disabled?: boolean,
	loading?: boolean,
	color?: "white" | "blue" | "green" | "orange" | "purple" | "red" | "blueGray",
	onClick?: React.EventHandler<React.MouseEvent<HTMLButtonElement>>,
	children?: any,
	className?: string,
	type?: string,
	formNoValidate?: boolean,
	id?: string,
	tabIndex?: number,
	style?: React.CSSProperties,
}): JSX.Element {
	const {
		block = false,
		outline = false,
		size,
		disabled = false,
		loading = false,
		color = "blueGray",
		onClick,
		className,
		children,
		style,
	} = props;
	const other = Object.assign({}, props);
	delete other.block;
	delete other.outline;
	delete other.size;
	delete other.disabled;
	delete other.loading;
	delete other.color;
	delete other.onClick;
	delete other.children;
	delete other.className;

	const btnColor = colors[color]();
	const btnHoverColor = color === "white" ? colors.blueD1() : colors[`${color}D1`]();
	const btnActiveColor = color === "white" ? colors.blueD2() : colors[`${color}D2`]();

	const sizeSx = {
		tiny: css({
			paddingBottom: "0.14rem",
			paddingTop: "0.18rem",
			paddingLeft: whitespace[2],
			paddingRight: whitespace[2],
		}, typography.size[7]),
		small: css({
			paddingBottom: whitespace[1],
			paddingTop: whitespace[1],
		}, typography.size[6]),
		large: css({
			paddingBottom: whitespace[2],
			paddingTop: whitespace[2],
			paddingLeft: whitespace[3],
			paddingRight: whitespace[3],
		}, typography.size[4]),
	};

	const outlineSx = css(
		{
			backgroundColor: "transparent",
			borderColor: color === "blueGray" ? colors.blueGrayL2(0.6) : btnColor,
			color: color === "blueGray" ? colors.blue() : btnColor,
		},
		{
			":hover": {
				backgroundColor: "transparent",
				borderColor: color === "blueGray" ? colors.blueGrayL2() : btnHoverColor,
				color: color === "blueGray" ? colors.blueD1() : btnHoverColor,
			}
		},
		{
			":active": {
				backgroundColor: "transparent",
				borderColor: btnActiveColor,
				color: btnActiveColor,
			}
		},

	);

	const whiteSx = css(
		{
			color: colors.blue(),
			backgroundColor: "white",
		},
		{
			":hover":
			{
				backgroundColor: "white",
				color: btnHoverColor,
			},
		},
		{ ":active": { color: btnActiveColor } }
	);

	return <button
		{...(other as any) }
		{...css(
			{
				backgroundColor: btnColor,
				borderWidth: 2,
				borderStyle: "solid",
				borderColor: "transparent",
				color: "white",
				textAlign: "center",
				fontWeight: "bold",
				outline: "none",
				paddingLeft: whitespace[3],
				paddingRight: whitespace[3],
				paddingTop: "0.45rem",
				paddingBottom: "0.42rem",
				borderRadius: 4,
				boxSizing: "border-box",
				cursor: "pointer",
				transition: "all 0.4s",
				userSelect: "none",

				display: block ? "block" : "inline-block",
				width: block ? "100%" : "auto",
			},
			{ ":hover": { backgroundColor: btnHoverColor } },
			{ ":active": { backgroundColor: btnActiveColor } },
			sizeSx[size as string],
			outline ? outlineSx : {},
			color === "white" ? whiteSx : {},
			{
				"[disabled]": {
					backgroundColor: colors.blueGrayL2(),
					color: colors.blueGray(0.7),
					cursor: "not-allowed",
				}
			}
		)
		}
		className={className}
		disabled={disabled}
		style={style}
		onClick={onClick}>
		{loading && <Loader {...props} />}
		{!loading && children}
	</button>;
}

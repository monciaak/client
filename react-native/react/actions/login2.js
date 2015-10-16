'use strict'

import * as Constants from '../constants/login2'
import QRCodeGen from 'qrcode-generator'
import { navigateTo } from './router'

export function welcomeSubmitUserPass (username, passphrase) {
  return {
    type: Constants.actionSubmitUserPass,
    username,
    passphrase
  }
}

function defaultModeForRoles (myRole, otherRole) {
  switch (myRole + otherRole) {
    case Constants.codePageRoleComputer1 + Constants.codePageRoleComputer2:
      return Constants.codePageModeEnterText
    case Constants.codePageRoleComputer2 + Constants.codePageRoleComputer1:
      return Constants.codePageModeShowText

    case Constants.codePageRoleComputer1 + Constants.codePageRolePhone2:
      return Constants.codePageModeShowCode
    case Constants.codePageRolePhone2 + Constants.codePageRoleComputer1:
      return Constants.codePageModeScanCode

    case Constants.codePageRolePhone1 + Constants.codePageRoleComputer2:
      return Constants.codePageModeScanCode
    case Constants.codePageRoleComputer2 + Constants.codePageRolePhone1:
      return Constants.codePageModeShowCode

    case Constants.codePageRolePhone1 + Constants.codePageRolePhone2:
      return Constants.codePageModeScanCode
    case Constants.codePageRolePhone2 + Constants.codePageRolePhone1:
      return Constants.codePageModeShowCode
  }
  return null
}

export function setCodePageRoles (myRole, otherRole) {
  return function (dispatch) {
    dispatch(setCodePageMode(defaultModeForRoles(myRole, otherRole)))
    dispatch({
      type: Constants.setCodeState,
      myRole,
      otherRole
    })
  }
}

let timerId = null
function resetCountdown () {
  clearInterval(timerId)
  timerId = null
}

// Count down until 0, then make a new code
function startCodeGenCountdown (mode) {
  let countDown = Constants.countDownTime

  return function (dispatch) {
    resetCountdown()
    timerId = setInterval(() => {
      countDown -= 1

      if (countDown <= 0) {
        dispatch(startCodeGen(mode))
      } else {
        dispatch({
          type: Constants.setCountdown,
          countDown
        })
      }
    }, 1000)

    dispatch({
      type: Constants.setCountdown,
      countDown
    })
  }
}

export function startCodeGen (mode) {
  return function (dispatch) {
    switch (mode) {
      case Constants.codePageModeShowText:
        dispatch({
          type: Constants.setTextCode,
          // TODO need this from go
          text: 'TODO TEMP:' + Math.floor(Math.random() * 99999)
        })
        dispatch(startCodeGenCountdown(mode))
    }
  }
}

export function setCodePageMode (mode) {
  resetCountdown()

  return function (dispatch) {
    dispatch(startCodeGen(mode))

    dispatch({
      type: Constants.setCodeMode,
      mode
    })
  }
}

export function qrScanned (code) {
  return function (dispatch) {
    // TODO send to go to verify
    console.log('QR Scanned: ', code)
    dispatch(navigateTo([]))
  }
}

export function qrGenerate () {
  return function (dispatch) {
    dispatch({
      type: Constants.qrGenerate
    })

    const qr = QRCodeGen(10, 'L')
    qr.addData(this.state.code)
    qr.make()
    let tag = qr.createImgTag(10)
    const [ , src, , ] = tag.split(' ')
    const [ , qrCode ] = src.split('\"')

    dispatch({
      type: Constants.qrGenerated,
      qrCode
    })
  }
}
